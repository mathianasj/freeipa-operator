/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	securityv1 "github.com/openshift/api/security/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	freeipav1alpha1 "github.com/mathianasj/freeipa-operator/api/v1alpha1"
	"github.com/mathianasj/freeipa-operator/internal/manifests"
)

const (
	finalizerName = "freeipa.mathianasj.io/finalizer"
	// requeuePeriod is used to poll for external changes such as the
	// LoadBalancer IP allocation and pod readiness.
	requeuePeriod = 60 * time.Second
	// requeuePeriodFast is used while waiting on the LoadBalancer IP.
	requeuePeriodFast = 15 * time.Second
)

// FreeIPAReconciler reconciles a FreeIPA object.
type FreeIPAReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Config        *rest.Config
	IngressDomain string
}

// +kubebuilder:rbac:groups=freeipa.mathianasj.io,resources=freeipas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=freeipa.mathianasj.io,resources=freeipas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=freeipa.mathianasj.io,resources=freeipas/finalizers,verbs=update

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=dnses,verbs=get

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;create;update
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile moves the current cluster state towards the state described by a
// FreeIPA object.
func (r *FreeIPAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	instance := &freeipav1alpha1.FreeIPA{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion: clean up the cluster-scoped SCC before finalizing.
	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, instance)
	}

	if !controllerutil.ContainsFinalizer(instance, finalizerName) {
		controllerutil.AddFinalizer(instance, finalizerName)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.initIngressDomain(ctx); err != nil {
		logger.Error(err, "failed to read the cluster ingress domain")
		return ctrl.Result{}, err
	}

	result, err := r.reconcileResources(ctx, instance)
	if err != nil {
		logger.Error(err, "failed to reconcile resources")
		return result, err
	}

	return result, nil
}

func (r *FreeIPAReconciler) reconcileDelete(ctx context.Context, instance *freeipav1alpha1.FreeIPA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(instance, finalizerName) {
		// Cluster-scoped resources cannot be garbage collected through owner
		// references, so remove the SCC here.
		scc := &securityv1.SecurityContextConstraints{}
		if err := r.Get(ctx, types.NamespacedName{Name: manifests.SCCName(instance)}, scc); err == nil {
			if err := r.Delete(ctx, scc); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("removed SecurityContextConstraints", "name", manifests.SCCName(instance))
		}
		controllerutil.RemoveFinalizer(instance, finalizerName)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// initIngressDomain caches the cluster ingress domain. It is needed for every
// instance: it derives the default host name when spec.host and spec.domain
// are unset and the hostname the Ingress Operator assigns to the route
// exposing the web UI otherwise.
func (r *FreeIPAReconciler) initIngressDomain(ctx context.Context) error {
	if r.IngressDomain != "" {
		return nil
	}
	ingress := &configv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, ingress); err != nil {
		return err
	}
	r.IngressDomain = ingress.Spec.Domain
	if r.IngressDomain == "" {
		return fmt.Errorf("the cluster ingress domain is empty")
	}
	return nil
}

func (r *FreeIPAReconciler) reconcileResources(ctx context.Context, m *freeipav1alpha1.FreeIPA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	lbPending := false

	if err := r.reconcileSecret(ctx, m); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileSCC(ctx, m); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileHostnameConfigMap(ctx, m); err != nil {
		return ctrl.Result{}, err
	}

	tcpIP, udpIP, err := r.reconcileServices(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tcpIP == "" {
		logger.Info("load balancer IP not allocated yet, waiting")
		r.setCondition(m, freeipav1alpha1.ConditionLoadBalancerPending, metav1.ConditionTrue,
			"LoadBalancerPending", "Waiting for the load balancer to allocate an external IP")
		r.setPhase(m, freeipav1alpha1.PhaseProvisioning)
		m.Status.Master = manifests.StatefulSetName(m)
		lbPending = true
	} else {
		r.setCondition(m, freeipav1alpha1.ConditionLoadBalancerPending, metav1.ConditionFalse,
			"LoadBalancerReady", fmt.Sprintf("External IP %s allocated", tcpIP))
	}

	if err := r.reconcileHostRecord(ctx, m, tcpIP); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileUDPSRVRecords(ctx, m, udpIP); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatefulSet(ctx, m, tcpIP); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileWebProxy(ctx, m); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileRoute(ctx, m); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, m, tcpIP, udpIP, lbPending); err != nil {
		return ctrl.Result{}, err
	}

	if lbPending {
		return ctrl.Result{RequeueAfter: requeuePeriodFast}, nil
	}
	return ctrl.Result{RequeueAfter: requeuePeriod}, nil
}

// reconcileSecret ensures the secret holding the FreeIPA passwords exists.
func (r *FreeIPAReconciler) reconcileSecret(ctx context.Context, m *freeipav1alpha1.FreeIPA) error {
	logger := log.FromContext(ctx)

	// An explicitly referenced secret is validated but never created.
	if m.Spec.PasswordSecret != nil {
		existing := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: *m.Spec.PasswordSecret}, existing); err != nil {
			return err
		}
		for _, key := range []string{"IPA_ADMIN_PASSWORD", "IPA_DM_PASSWORD"} {
			if _, ok := existing.Data[key]; !ok {
				return fmt.Errorf("secret %q is missing the %q key", *m.Spec.PasswordSecret, key)
			}
		}
		return nil
	}

	name := manifests.SecretName(m)
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("creating secret with random passwords", "secret", name)
	secret := manifests.SecretForFreeIPA(m, "", "")
	if err := controllerutil.SetControllerReference(m, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// reconcileSCC ensures the SecurityContextConstraints used by the FreeIPA
// workload exists.
func (r *FreeIPAReconciler) reconcileSCC(ctx context.Context, m *freeipav1alpha1.FreeIPA) error {
	name := manifests.SCCName(m)
	existing := &securityv1.SecurityContextConstraints{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, existing)
	if err == nil {
		desired := manifests.SCCForFreeIPA(m)
		if sccNeedsUpdate(existing, desired) {
			logger := log.FromContext(ctx)
			logger.Info("updating SecurityContextConstraints", "name", name)
			desired.ResourceVersion = existing.ResourceVersion
			return r.Update(ctx, desired)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, manifests.SCCForFreeIPA(m))
}

func sccNeedsUpdate(existing, desired *securityv1.SecurityContextConstraints) bool {
	return existing.AllowPrivilegedContainer != desired.AllowPrivilegedContainer ||
		!reflect.DeepEqual(existing.Users, desired.Users)
}

// reconcileHostnameConfigMap ensures the config map pinning /etc/hostname to
// the instance FQDN exists and matches the current host.
func (r *FreeIPAReconciler) reconcileHostnameConfigMap(ctx context.Context, m *freeipav1alpha1.FreeIPA) error {
	logger := log.FromContext(ctx)
	name := manifests.HostnameConfigMapName(m)
	desired := manifests.HostnameConfigMapForFreeIPA(m, manifests.HostForFreeIPA(m, r.IngressDomain))

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, existing)
	if err == nil {
		if existing.Data["hostname"] == desired.Data["hostname"] {
			return nil
		}
		logger.Info("updating hostname config map", "name", name)
		desired.ResourceVersion = existing.ResourceVersion
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return err
		}
		return r.Update(ctx, desired)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	logger.Info("creating hostname config map", "name", name)
	if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, desired)
}

// reconcileServices ensures the LoadBalancer services exist and returns the
// allocated external IPs. When spec.udpService is configured a second,
// UDP-only service is created as well.
func (r *FreeIPAReconciler) reconcileServices(ctx context.Context, m *freeipav1alpha1.FreeIPA) (string, string, error) {
	tcpIP, err := r.ensureService(ctx, m, manifests.ServiceName(m), manifests.ServiceForFreeIPA(m))
	if err != nil {
		return "", "", err
	}
	udpIP := ""
	if m.Spec.UDPService != nil {
		udpIP, err = r.ensureService(ctx, m, manifests.UDPServiceName(m), manifests.UDPServiceForFreeIPA(m))
		if err != nil {
			return "", "", err
		}
	}
	return tcpIP, udpIP, nil
}

func (r *FreeIPAReconciler) ensureService(ctx context.Context, m *freeipav1alpha1.FreeIPA, name string, desired *corev1.Service) (string, error) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, svc); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return "", err
		}
		if err := r.Create(ctx, desired); err != nil {
			return "", err
		}
		return "", nil
	}
	return loadBalancerIP(svc), nil
}

func loadBalancerIP(svc *corev1.Service) string {
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return ""
}

// reconcileHostRecord points the instance hostname in the integrated DNS zone
// at the LoadBalancer endpoint so that Kerberos, LDAP and DNS clients outside
// the cluster can reach the server. FreeIPA installs the master host record
// with the pod IP, which is not reachable off-cluster. When the LoadBalancer
// endpoint is a hostname (e.g. an AWS NLB) a CNAME record is used, otherwise a
// plain A record (e.g. MetalLB).
func (r *FreeIPAReconciler) reconcileHostRecord(ctx context.Context, m *freeipav1alpha1.FreeIPA, lbIP string) error {
	if m.Spec.DNS == nil || lbIP == "" {
		return nil
	}

	name, zone, err := manifests.HostRecord(m, r.IngressDomain)
	if err != nil {
		return err
	}

	podName := manifests.StatefulSetName(m) + "-0"
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !podReady(pod) {
		return nil
	}

	pass, err := r.adminPassword(ctx, m)
	if err != nil {
		return err
	}

	cname := ""
	if net.ParseIP(lbIP) == nil {
		cname = lbIP
	}

	out, err := r.execOutput(ctx, m.Namespace, podName, []string{"sh", "-c", hostRecordScript(name, zone, cname, lbIP, pass)})
	if err != nil {
		logger := log.FromContext(ctx)
		if strings.Contains(err.Error(), "exec produced no output") {
			// Exec streams through the API proxy occasionally return an
			// empty payload; the record is reconciled again on the next
			// periodic pass.
			logger.V(1).Info("hostname DNS record reconcile deferred", "error", err.Error())
		} else {
			logger.Info("hostname DNS record reconcile deferred", "error", err.Error(), "output", out)
		}
		return nil
	}
	if !strings.Contains(out, "nochange") {
		logger := log.FromContext(ctx)
		logger.Info("updated hostname DNS record", "record", name+"."+zone, "target", lbIP)
	}
	return nil
}

// reconcileUDPSRVRecords keeps the UDP SRV records in the integrated DNS zone
// aligned with the UDP LoadBalancer service so they always match the created
// load balancers. When spec.udpService is configured and the UDP endpoint is
// allocated, the operator-owned <host>-udp record is pointed at the UDP
// endpoint and the UDP SRV records (_kerberos._udp, _kerberos-master._udp,
// _kpasswd._udp) are repointed at it. Otherwise the record is removed and the
// SRV targets are restored to the instance hostname.
func (r *FreeIPAReconciler) reconcileUDPSRVRecords(ctx context.Context, m *freeipav1alpha1.FreeIPA, udpIP string) error {
	if m.Spec.DNS == nil {
		return nil
	}

	name, zone, err := manifests.HostRecord(m, r.IngressDomain)
	if err != nil {
		return err
	}
	udpName, _, err := manifests.UDPRecord(m, r.IngressDomain)
	if err != nil {
		return err
	}

	podName := manifests.StatefulSetName(m) + "-0"
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !podReady(pod) {
		return nil
	}

	pass, err := r.adminPassword(ctx, m)
	if err != nil {
		return err
	}

	// Without a configured and allocated UDP LoadBalancer the SRV records
	// keep pointing at the instance hostname and the UDP record is removed.
	udpCname := ""
	udpARec := ""
	srvTarget := name + "." + zone
	if m.Spec.UDPService != nil && udpIP != "" {
		if net.ParseIP(udpIP) == nil {
			udpCname = udpIP
		} else {
			udpARec = udpIP
		}
		srvTarget = udpName + "." + zone
	}

	out, err := r.execOutput(ctx, m.Namespace, podName, []string{"sh", "-c", udpEndpointScript(zone, udpName, udpCname, udpARec, srvTarget, pass)})
	if err != nil {
		logger := log.FromContext(ctx)
		logger.V(1).Info("UDP SRV record reconcile deferred", "error", err.Error(), "output", out)
		return nil
	}
	if !strings.Contains(out, "nochange") {
		logger := log.FromContext(ctx)
		logger.Info("updated UDP SRV records", "record", udpName+"."+zone, "target", srvTarget)
	}
	return nil
}

// adminPassword returns the FreeIPA administrator password, reading it from
// the referenced secret or the operator-generated one.
func (r *FreeIPAReconciler) adminPassword(ctx context.Context, m *freeipav1alpha1.FreeIPA) (string, error) {
	name := manifests.SecretName(m)
	if m.Spec.PasswordSecret != nil {
		name = *m.Spec.PasswordSecret
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, secret); err != nil {
		return "", err
	}
	pass, ok := secret.Data["IPA_ADMIN_PASSWORD"]
	if !ok {
		return "", fmt.Errorf("secret %q is missing the IPA_ADMIN_PASSWORD key", name)
	}
	return string(pass), nil
}

// hostRecordScript returns a shell script that reconciles the A or CNAME
// record for the instance hostname in the integrated DNS zone to match the
// desired LoadBalancer endpoint. The admin password is embedded base64-encoded
// so the secret never needs shell quoting. The script always prints a status
// line ("nochange", "updated" or an error) and exits non-zero on failure so
// the operator can distinguish a real update from a transient exec problem.
func hostRecordScript(name, zone, cname, aRec, pass string) string {
	passB64 := base64.StdEncoding.EncodeToString([]byte(pass))
	records := fmt.Sprintf(`existing_cname=$(ipa dnsrecord-show "%s" "%s" 2>/dev/null | sed -n 's/^  CNAME record: //p')
existing_a=$(ipa dnsrecord-show "%s" "%s" 2>/dev/null | sed -n 's/^  A record: //p')
`, zone, name, zone, name)
	if cname != "" {
		records += fmt.Sprintf(`if [ "$existing_cname" = "%s." ]; then
  echo nochange
  exit 0
fi
if [ -n "$existing_cname" ]; then
  ipa dnsrecord-del "%s" "%s" --cname-rec="$existing_cname" >/dev/null || { echo "error deleting CNAME"; exit 1; }
fi
if [ -n "$existing_a" ]; then
  ipa dnsrecord-del "%s" "%s" --a-rec="$existing_a" >/dev/null || { echo "error deleting A"; exit 1; }
fi
ipa dnsrecord-add "%s" "%s" --cname-rec="%s." >/dev/null || { echo "error adding CNAME"; exit 1; }
echo updated
`, cname, zone, name, zone, name, zone, name, cname)
	} else {
		records += fmt.Sprintf(`if [ "$existing_a" = "%s" ]; then
  echo nochange
  exit 0
fi
if [ -n "$existing_cname" ]; then
  ipa dnsrecord-del "%s" "%s" --cname-rec="$existing_cname" >/dev/null || { echo "error deleting CNAME"; exit 1; }
fi
if [ -n "$existing_a" ]; then
  ipa dnsrecord-del "%s" "%s" --a-rec="$existing_a" >/dev/null || { echo "error deleting A"; exit 1; }
fi
ipa dnsrecord-add "%s" "%s" --a-rec="%s" >/dev/null || { echo "error adding A"; exit 1; }
echo updated
`, aRec, zone, name, zone, name, zone, name, aRec)
	}
	return fmt.Sprintf(`if ! kinit -R admin >/dev/null 2>&1; then
  if ! echo %s | base64 -d | kinit admin >/dev/null 2>&1; then
    echo "error kinit failed"
    exit 1
  fi
fi
%s`, passB64, records)
}

// udpEndpointScript returns a shell script that keeps the UDP DNS state aligned
// with the UDP LoadBalancer service. When udpCname or udpARec is set, the
// operator-owned <host>-udp record is created/updated to point at the UDP
// endpoint and the UDP SRV records (_kerberos._udp, _kerberos-master._udp,
// _kpasswd._udp) are repointed at it, preserving priority, weight and port.
// When both endpoint fields are empty the record is removed and the SRV
// targets are restored to the instance hostname, matching the state where no
// UDP LoadBalancer exists. The script always prints "nochange", "updated" or
// an error and exits non-zero on failure.
func udpEndpointScript(zone, udpName, udpCname, udpARec, srvTarget, pass string) string {
	passB64 := base64.StdEncoding.EncodeToString([]byte(pass))
	srvs := "_kerberos._udp _kerberos-master._udp _kpasswd._udp"
	script := fmt.Sprintf(`if ! kinit -R admin >/dev/null 2>&1; then
  if ! echo %s | base64 -d | kinit admin >/dev/null 2>&1; then
    echo "error kinit failed"
    exit 1
  fi
fi
changed=0
`, passB64)
	if udpCname != "" {
		script += fmt.Sprintf(`existing_cname=$(ipa dnsrecord-show "%s" "%s" 2>/dev/null | sed -n 's/^  CNAME record: //p')
existing_a=$(ipa dnsrecord-show "%s" "%s" 2>/dev/null | sed -n 's/^  A record: //p')
if [ "$existing_cname" != "%s." ]; then
  if [ -n "$existing_cname" ]; then
    ipa dnsrecord-del "%s" "%s" --cname-rec="$existing_cname" >/dev/null || { echo "error deleting CNAME"; exit 1; }
  fi
  if [ -n "$existing_a" ]; then
    ipa dnsrecord-del "%s" "%s" --a-rec="$existing_a" >/dev/null || { echo "error deleting A"; exit 1; }
  fi
  ipa dnsrecord-add "%s" "%s" --cname-rec="%s." >/dev/null || { echo "error adding CNAME"; exit 1; }
  changed=1
fi
`, zone, udpName, zone, udpName, udpCname, zone, udpName, zone, udpName, zone, udpName, udpCname)
	} else if udpARec != "" {
		script += fmt.Sprintf(`existing_cname=$(ipa dnsrecord-show "%s" "%s" 2>/dev/null | sed -n 's/^  CNAME record: //p')
existing_a=$(ipa dnsrecord-show "%s" "%s" 2>/dev/null | sed -n 's/^  A record: //p')
if [ "$existing_a" != "%s" ]; then
  if [ -n "$existing_cname" ]; then
    ipa dnsrecord-del "%s" "%s" --cname-rec="$existing_cname" >/dev/null || { echo "error deleting CNAME"; exit 1; }
  fi
  if [ -n "$existing_a" ]; then
    ipa dnsrecord-del "%s" "%s" --a-rec="$existing_a" >/dev/null || { echo "error deleting A"; exit 1; }
  fi
  ipa dnsrecord-add "%s" "%s" --a-rec="%s" >/dev/null || { echo "error adding A"; exit 1; }
  changed=1
fi
`, zone, udpName, zone, udpName, udpARec, zone, udpName, zone, udpName, zone, udpName, udpARec)
	} else {
		// No UDP LoadBalancer: the operator-owned record must not exist.
		script += fmt.Sprintf(`if ipa dnsrecord-show "%s" "%s" >/dev/null 2>&1; then
  ipa dnsrecord-del "%s" "%s" --del-all >/dev/null || { echo "error deleting UDP record"; exit 1; }
  changed=1
fi
`, zone, udpName, zone, udpName)
	}
	// Repoint the UDP SRV records at srvTarget, preserving their other fields.
	script += fmt.Sprintf(`for rec in %s; do
  ipa dnsrecord-show "%s" "$rec" 2>/dev/null | sed -n 's/^  SRV record: //p' > /tmp/ipa_srv.$$
  while read -r prio weight port target; do
    [ -z "$target" ] && continue
    if [ "$target" != "%s." ]; then
      ipa dnsrecord-del "%s" "$rec" --srv-rec="$prio $weight $port $target" >/dev/null || { rm -f /tmp/ipa_srv.$$; echo "error deleting SRV"; exit 1; }
      ipa dnsrecord-add "%s" "$rec" --srv-rec="$prio $weight $port %s." >/dev/null || { rm -f /tmp/ipa_srv.$$; echo "error adding SRV"; exit 1; }
      changed=1
    fi
  done < /tmp/ipa_srv.$$
  rm -f /tmp/ipa_srv.$$
done
`, srvs, zone, srvTarget, zone, zone, srvTarget)
	script += `if [ "$changed" = "1" ]; then
  echo updated
else
  echo nochange
fi`
	return script
}

// reconcileStatefulSet creates the StatefulSet running the FreeIPA server and
// updates it when the workload definition changes.
func (r *FreeIPAReconciler) reconcileStatefulSet(ctx context.Context, m *freeipav1alpha1.FreeIPA, lbIP string) error {
	logger := log.FromContext(ctx)
	desired := manifests.StatefulSetForFreeIPA(m, r.IngressDomain, lbIP)

	existing := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: manifests.StatefulSetName(m)}, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		logger.Info("creating statefulset", "name", manifests.StatefulSetName(m))
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}

	if statefulSetNeedsUpdate(existing, desired) {
		logger.Info("updating statefulset", "name", manifests.StatefulSetName(m))
		desired.ObjectMeta.ResourceVersion = existing.ObjectMeta.ResourceVersion
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return err
		}
		return r.Update(ctx, desired)
	}
	return nil
}

// statefulSetNeedsUpdate reports whether the running StatefulSet must be
// updated to match the desired definition.
func statefulSetNeedsUpdate(existing, desired *appsv1.StatefulSet) bool {
	if len(existing.Spec.Template.Spec.Containers) != len(desired.Spec.Template.Spec.Containers) {
		return true
	}
	for i := range desired.Spec.Template.Spec.Containers {
		got := existing.Spec.Template.Spec.Containers[i]
		want := desired.Spec.Template.Spec.Containers[i]
		if got.Image != want.Image {
			return true
		}
		if containerPrivileged(got) != containerPrivileged(want) {
			return true
		}
		if strings.Join(got.Command, " ") != strings.Join(want.Command, " ") {
			return true
		}
		if strings.Join(got.Args, " ") != strings.Join(want.Args, " ") {
			return true
		}
		if !resourceRequirementsEqual(got.Resources, want.Resources) {
			return true
		}
		if !envEqual(got.Env, want.Env) {
			return true
		}
		if !volumeMountsEqual(got.VolumeMounts, want.VolumeMounts) {
			return true
		}
	}
	if !volumesEqual(existing.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		return true
	}
	return false
}

func containerPrivileged(c corev1.Container) bool {
	if c.SecurityContext == nil {
		return false
	}
	return c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged
}

func resourceRequirementsEqual(a, b corev1.ResourceRequirements) bool {
	return a.Requests.Cpu().Cmp(*b.Requests.Cpu()) == 0 &&
		a.Requests.Memory().Cmp(*b.Requests.Memory()) == 0 &&
		a.Limits.Cpu().Cmp(*b.Limits.Cpu()) == 0 &&
		a.Limits.Memory().Cmp(*b.Limits.Memory()) == 0
}

func envEqual(a, b []corev1.EnvVar) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value {
			return false
		}
		if (a[i].ValueFrom == nil) != (b[i].ValueFrom == nil) {
			return false
		}
		if a[i].ValueFrom != nil && b[i].ValueFrom != nil {
			if (a[i].ValueFrom.FieldRef == nil) != (b[i].ValueFrom.FieldRef == nil) {
				return false
			}
			if a[i].ValueFrom.FieldRef != nil && b[i].ValueFrom.FieldRef != nil && a[i].ValueFrom.FieldRef.FieldPath != b[i].ValueFrom.FieldRef.FieldPath {
				return false
			}
			if (a[i].ValueFrom.SecretKeyRef == nil) != (b[i].ValueFrom.SecretKeyRef == nil) {
				return false
			}
			if a[i].ValueFrom.SecretKeyRef != nil && b[i].ValueFrom.SecretKeyRef != nil {
				if a[i].ValueFrom.SecretKeyRef.Name != b[i].ValueFrom.SecretKeyRef.Name || a[i].ValueFrom.SecretKeyRef.Key != b[i].ValueFrom.SecretKeyRef.Key {
					return false
				}
			}
		}
	}
	return true
}

func volumeMountsEqual(a, b []corev1.VolumeMount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].MountPath != b[i].MountPath || a[i].SubPath != b[i].SubPath {
			return false
		}
	}
	return true
}

func volumesEqual(a, b []corev1.Volume) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		va, vb := a[i], b[i]
		if va.Name != vb.Name {
			return false
		}
		if (va.ConfigMap == nil) != (vb.ConfigMap == nil) {
			return false
		}
		if va.ConfigMap != nil && va.ConfigMap.Name != vb.ConfigMap.Name {
			return false
		}
	}
	return true
}

// reconcileRoute ensures the route exposing the FreeIPA web UI exists and
// matches the desired TLS configuration. The operator-owned fields are
// reconciled; the generated host and any certificate/key injected by a user or
// cert-manager are preserved.
func (r *FreeIPAReconciler) reconcileRoute(ctx context.Context, m *freeipav1alpha1.FreeIPA) error {
	logger := log.FromContext(ctx)
	viaWeb := r.webProxyActive(m)

	destinationCA := ""
	if !viaWeb && m.Spec.Route != nil && m.Spec.Route.Termination == string(routev1.TLSTerminationReencrypt) {
		destinationCA = m.Spec.Route.DestinationCACertificate
		if destinationCA == "" {
			ca, err := r.fetchServerCACert(ctx, m)
			if err != nil {
				logger.Info("FreeIPA CA certificate not available yet, deferring route reconcile", "error", err.Error())
				return nil
			}
			destinationCA = ca
		}
	}

	route := &routev1.Route{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: manifests.RouteName(m)}, route); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		desired := manifests.RouteForFreeIPA(m, destinationCA, viaWeb)
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}

	desired := manifests.RouteForFreeIPA(m, destinationCA, viaWeb)
	if !routeNeedsUpdate(route, desired) {
		return nil
	}
	logger.Info("updating route", "name", manifests.RouteName(m))
	// Keep the host generated by OpenShift and any certificate material a
	// user or cert-manager injected on the route.
	if route.Spec.Host != "" {
		desired.Spec.Host = route.Spec.Host
	}
	desired.Spec.TLS.Certificate = route.Spec.TLS.Certificate
	desired.Spec.TLS.Key = route.Spec.TLS.Key
	desired.ObjectMeta.ResourceVersion = route.ObjectMeta.ResourceVersion
	if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, desired)
}

// webProxyActive reports whether the nginx reverse proxy is required in front
// of the FreeIPA web UI. FreeIPA hard-codes its own hostname into the
// redirects and absolute URLs it emits; when the public route hostname differs
// from the instance hostname the proxy rewrites them on the fly.
func (r *FreeIPAReconciler) webProxyActive(m *freeipav1alpha1.FreeIPA) bool {
	return manifests.HostForFreeIPA(m, r.IngressDomain) != manifests.RouteHost(m, r.IngressDomain)
}

// reconcileWebProxy ensures the nginx reverse proxy fronting the FreeIPA web
// UI exists when the public route hostname differs from the instance hostname
// and removes it when it is no longer needed.
func (r *FreeIPAReconciler) reconcileWebProxy(ctx context.Context, m *freeipav1alpha1.FreeIPA) error {
	if !r.webProxyActive(m) {
		return r.deleteWebProxy(ctx, m)
	}

	logger := log.FromContext(ctx)
	host := manifests.HostForFreeIPA(m, r.IngressDomain)
	backend := manifests.WebBackend(m)
	conf := manifests.WebConfigForFreeIPA(m, host, backend)

	if err := r.reconcileWebConfigMap(ctx, m, host, backend); err != nil {
		return err
	}
	if err := r.reconcileWebDeployment(ctx, m, conf); err != nil {
		return err
	}
	if err := r.reconcileWebService(ctx, m); err != nil {
		return err
	}

	logger.Info("web proxy serving the FreeIPA web UI",
		"deployment", manifests.WebDeploymentName(m), "service", manifests.WebServiceName(m))
	return nil
}

func (r *FreeIPAReconciler) reconcileWebConfigMap(ctx context.Context, m *freeipav1alpha1.FreeIPA, host, backend string) error {
	logger := log.FromContext(ctx)
	name := manifests.WebConfigMapName(m)
	desired := manifests.WebConfigMapForFreeIPA(m, host, backend)

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, existing)
	if err == nil {
		if existing.Data["default.conf"] == desired.Data["default.conf"] {
			return nil
		}
		logger.Info("updating web proxy config map", "name", name)
		desired.ResourceVersion = existing.ResourceVersion
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return err
		}
		return r.Update(ctx, desired)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	logger.Info("creating web proxy config map", "name", name)
	if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, desired)
}

func (r *FreeIPAReconciler) reconcileWebDeployment(ctx context.Context, m *freeipav1alpha1.FreeIPA, conf string) error {
	logger := log.FromContext(ctx)
	desired := manifests.WebDeploymentForFreeIPA(m, conf)

	existing := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: manifests.WebDeploymentName(m)}, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		logger.Info("creating web proxy deployment", "name", manifests.WebDeploymentName(m))
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}

	if !deploymentNeedsUpdate(existing, desired) {
		return nil
	}
	logger.Info("updating web proxy deployment", "name", manifests.WebDeploymentName(m))
	desired.ObjectMeta.ResourceVersion = existing.ObjectMeta.ResourceVersion
	if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, desired)
}

func (r *FreeIPAReconciler) reconcileWebService(ctx context.Context, m *freeipav1alpha1.FreeIPA) error {
	logger := log.FromContext(ctx)
	desired := manifests.WebServiceForFreeIPA(m)

	existing := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: manifests.WebServiceName(m)}, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		logger.Info("creating web proxy service", "name", manifests.WebServiceName(m))
		if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}

	if !serviceNeedsUpdate(existing, desired) {
		return nil
	}
	logger.Info("updating web proxy service", "name", manifests.WebServiceName(m))
	desired.ObjectMeta.ResourceVersion = existing.ObjectMeta.ResourceVersion
	if err := controllerutil.SetControllerReference(m, desired, r.Scheme); err != nil {
		return err
	}
	return r.Update(ctx, desired)
}

// deleteWebProxy removes the web proxy resources when the instance hostname
// matches the public route hostname again.
func (r *FreeIPAReconciler) deleteWebProxy(ctx context.Context, m *freeipav1alpha1.FreeIPA) error {
	for _, del := range []struct {
		kind string
		obj  client.Object
	}{
		{"deployment", &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: manifests.WebDeploymentName(m)}}},
		{"service", &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: manifests.WebServiceName(m)}}},
		{"config map", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: manifests.WebConfigMapName(m)}}},
	} {
		err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: del.obj.GetName()}, del.obj)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(del.obj, m) {
			continue
		}
		logger := log.FromContext(ctx)
		logger.Info("removing web proxy", "kind", del.kind, "name", del.obj.GetName())
		if err := r.Delete(ctx, del.obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// deploymentNeedsUpdate reports whether the running Deployment must be updated
// to match the desired definition.
func deploymentNeedsUpdate(existing, desired *appsv1.Deployment) bool {
	if (existing.Spec.Replicas == nil) != (desired.Spec.Replicas == nil) {
		return true
	}
	if existing.Spec.Replicas != nil && desired.Spec.Replicas != nil && *existing.Spec.Replicas != *desired.Spec.Replicas {
		return true
	}
	if existing.Spec.Template.Annotations[manifests.WebConfigHashAnnotation] != desired.Spec.Template.Annotations[manifests.WebConfigHashAnnotation] {
		return true
	}
	if len(existing.Spec.Template.Spec.Containers) != len(desired.Spec.Template.Spec.Containers) {
		return true
	}
	for i := range desired.Spec.Template.Spec.Containers {
		got := existing.Spec.Template.Spec.Containers[i]
		want := desired.Spec.Template.Spec.Containers[i]
		if got.Image != want.Image {
			return true
		}
		if !containerPortsEqual(got.Ports, want.Ports) {
			return true
		}
		if !volumeMountsEqual(got.VolumeMounts, want.VolumeMounts) {
			return true
		}
	}
	if !volumesEqual(existing.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		return true
	}
	return false
}

func containerPortsEqual(a, b []corev1.ContainerPort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].ContainerPort != b[i].ContainerPort {
			return false
		}
	}
	return true
}

// serviceNeedsUpdate reports whether the running Service must be updated to
// match the desired definition.
func serviceNeedsUpdate(existing, desired *corev1.Service) bool {
	if !reflect.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) {
		return true
	}
	if len(existing.Spec.Ports) != len(desired.Spec.Ports) {
		return true
	}
	for i := range desired.Spec.Ports {
		if existing.Spec.Ports[i].Name != desired.Spec.Ports[i].Name ||
			existing.Spec.Ports[i].Port != desired.Spec.Ports[i].Port ||
			existing.Spec.Ports[i].TargetPort != desired.Spec.Ports[i].TargetPort {
			return true
		}
	}
	return false
}

// fetchServerCACert returns the PEM-encoded certificate of the FreeIPA CA that
// signs the server certificate, read from the running server pod. It returns
// an error when the pod is not ready or the certificate is not present yet.
func (r *FreeIPAReconciler) fetchServerCACert(ctx context.Context, m *freeipav1alpha1.FreeIPA) (string, error) {
	podName := manifests.StatefulSetName(m) + "-0"
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: podName}, pod); err != nil {
		return "", fmt.Errorf("fetching pod %s/%s: %w", m.Namespace, podName, err)
	}
	if !podReady(pod) {
		return "", fmt.Errorf("pod %s/%s is not ready", m.Namespace, podName)
	}
	for _, path := range []string{"/etc/ipa/ca.crt", "/data/ipa/ca.crt"} {
		out, err := r.execOutput(ctx, m.Namespace, podName, []string{"cat", path})
		if err == nil && strings.Contains(out, "BEGIN CERTIFICATE") {
			return out, nil
		}
	}
	return "", fmt.Errorf("FreeIPA CA certificate not found in pod %s/%s", m.Namespace, podName)
}

// podReady reports whether every container of the pod is ready.
func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// exec runs a command in the FreeIPA container of a pod and returns its
// combined output. Commands that legitimately produce no output return "".
// Exec streams through the API server can fail transiently, so failed attempts
// are retried with a fresh connection. Callers that need output should use
// execOutput instead, which additionally retries silently-truncated streams.
func (r *FreeIPAReconciler) exec(ctx context.Context, namespace, pod string, command []string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		clientset, err := kubernetes.NewForConfig(r.Config)
		if err != nil {
			return "", err
		}
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(namespace).
			Name(pod).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: "freeipa",
				Command:   command,
				Stdout:    true,
				Stderr:    true,
			}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(r.Config, "POST", req.URL())
		if err != nil {
			return "", err
		}
		var buf bytes.Buffer
		if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &buf, Stderr: &buf}); err != nil {
			lastErr = err
			if attempt < 3 {
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			continue
		}
		return buf.String(), nil
	}
	return "", lastErr
}

// execOutput is exec, but a successful stream that produces no output is
// treated as a failed attempt because some clusters silently truncate exec
// output.
func (r *FreeIPAReconciler) execOutput(ctx context.Context, namespace, pod string, command []string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		out, err := r.exec(ctx, namespace, pod, command)
		if err != nil {
			lastErr = err
		} else if out != "" {
			return out, nil
		} else {
			lastErr = fmt.Errorf("exec produced no output")
		}
		if attempt < 5 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	return "", lastErr
}

// routeNeedsUpdate reports whether the existing route must be updated to
// match the desired operator-owned TLS configuration and target service.
func routeNeedsUpdate(existing, desired *routev1.Route) bool {
	et, dt := existing.Spec.TLS.Termination, desired.Spec.TLS.Termination
	ec, dc := existing.Spec.TLS.DestinationCACertificate, desired.Spec.TLS.DestinationCACertificate
	ei, di := existing.Spec.TLS.InsecureEdgeTerminationPolicy, desired.Spec.TLS.InsecureEdgeTerminationPolicy
	if et != dt || ec != dc || ei != di {
		return true
	}
	if existing.Spec.To.Name != desired.Spec.To.Name {
		return true
	}
	if (existing.Spec.Port == nil) != (desired.Spec.Port == nil) {
		return true
	}
	if existing.Spec.Port != nil && desired.Spec.Port != nil && existing.Spec.Port.TargetPort != desired.Spec.Port.TargetPort {
		return true
	}
	return false
}

// updateStatus persists the observed state of the instance.
func (r *FreeIPAReconciler) updateStatus(ctx context.Context, m *freeipav1alpha1.FreeIPA, tcpIP, udpIP string, lbPending bool) error {
	m.Status.SecretName = manifests.SecretName(m)
	m.Status.Master = manifests.StatefulSetName(m)
	m.Status.LoadBalancerIP = tcpIP
	m.Status.UDPLoadBalancerIP = udpIP

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: manifests.StatefulSetName(m)}, sts); err == nil {
		if sts.Status.ReadyReplicas >= 1 {
			r.setCondition(m, freeipav1alpha1.ConditionAvailable, metav1.ConditionTrue,
				"FreeIPAReady", "The FreeIPA server is reporting Ready")
			r.setCondition(m, freeipav1alpha1.ConditionProgressing, metav1.ConditionFalse,
				"FreeIPAProvisioned", "The FreeIPA server is provisioned")
			r.setPhase(m, freeipav1alpha1.PhaseReady)
		} else {
			r.setCondition(m, freeipav1alpha1.ConditionAvailable, metav1.ConditionFalse,
				"FreeIPAProvisioning", "The FreeIPA server is still being provisioned")
			r.setCondition(m, freeipav1alpha1.ConditionProgressing, metav1.ConditionTrue,
				"FreeIPAProvisioning", "The FreeIPA server is being provisioned")
			r.setPhase(m, freeipav1alpha1.PhaseProvisioning)
		}
	} else if apierrors.IsNotFound(err) {
		r.setCondition(m, freeipav1alpha1.ConditionProgressing, metav1.ConditionTrue,
			"FreeIPAProvisioning", "The FreeIPA StatefulSet does not exist yet")
		r.setPhase(m, freeipav1alpha1.PhaseProvisioning)
	} else {
		return err
	}

	if lbPending {
		r.setPhase(m, freeipav1alpha1.PhaseProvisioning)
	}

	if err := r.Status().Update(ctx, m); err != nil {
		return err
	}
	return nil
}

func (r *FreeIPAReconciler) setPhase(m *freeipav1alpha1.FreeIPA, phase string) {
	m.Status.Phase = phase
}

func (r *FreeIPAReconciler) setCondition(m *freeipav1alpha1.FreeIPA, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range m.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				c.Status = status
				c.LastTransitionTime = now
			}
			c.Reason = reason
			c.Message = message
			c.ObservedGeneration = m.Generation
			m.Status.Conditions[i] = c
			return
		}
	}
	m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: m.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *FreeIPAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&freeipav1alpha1.FreeIPA{}).
		Complete(r)
}
