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

// Package manifests builds the Kubernetes/OpenShift resources that back a
// FreeIPA custom resource.
package manifests

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"math/rand"
	"net"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	routev1 "github.com/openshift/api/route/v1"
	securityv1 "github.com/openshift/api/security/v1"

	freeipav1alpha1 "github.com/mathianasj/freeipa-operator/api/v1alpha1"
)

// LabelsForFreeIPA returns the labels used to select the resources that belong
// to the given FreeIPA instance.
func LabelsForFreeIPA(m *freeipav1alpha1.FreeIPA) map[string]string {
	return map[string]string{
		"app":     "freeipa",
		"freeipa": m.Name,
	}
}

// GenerateRandomPassword returns a random password following the
// XXXXX-XXXXX-XXXXX-XXXXX pattern.
func GenerateRandomPassword() string {
	return randStringBytes(5) + "-" + randStringBytes(5) + "-" + randStringBytes(5) + "-" + randStringBytes(5)
}

func randStringBytes(n int) string {
	const letterBytes = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

// DefaultHost returns the default fully qualified hostname for the instance:
// <name>-<namespace>.<domain>. The domain is spec.domain when set, otherwise
// the cluster ingress domain.
func DefaultHost(m *freeipav1alpha1.FreeIPA, ingressDomain string) string {
	return m.Name + "-" + m.Namespace + "." + effectiveDomain(m, ingressDomain)
}

// effectiveDomain returns the root domain used to derive the default host,
// preferring the per-instance override over the cluster ingress domain.
func effectiveDomain(m *freeipav1alpha1.FreeIPA, ingressDomain string) string {
	if m.Spec.Domain != "" {
		return m.Spec.Domain
	}
	return ingressDomain
}

// HostForFreeIPA returns the fully qualified hostname of the instance,
// defaulting to the generated host when the spec does not override it.
func HostForFreeIPA(m *freeipav1alpha1.FreeIPA, ingressDomain string) string {
	if m.Spec.Host != "" {
		return m.Spec.Host
	}
	return DefaultHost(m, ingressDomain)
}

// RouteHost returns the hostname the OpenShift Ingress Operator assigns to the
// route exposing the instance: <route-name>-<namespace>.<ingress-domain>. The
// route is created without a host so OpenShift derives it from the cluster
// ingress domain, which can differ from the instance hostname when spec.domain
// or spec.host override it.
func RouteHost(m *freeipav1alpha1.FreeIPA, ingressDomain string) string {
	return m.Name + "-" + m.Namespace + "." + ingressDomain
}

// HostRecord returns the owner name and zone of the DNS record hosting the
// instance hostname in the integrated DNS zone, used to repoint it at the
// LoadBalancer endpoint.
func HostRecord(m *freeipav1alpha1.FreeIPA, ingressDomain string) (name, zone string, err error) {
	fqdn := HostForFreeIPA(m, ingressDomain)
	i := strings.IndexByte(fqdn, '.')
	if i <= 0 || i == len(fqdn)-1 {
		return "", "", fmt.Errorf("host %q has no DNS zone", fqdn)
	}
	return fqdn[:i], fqdn[i+1:], nil
}

// UDPRecord returns the owner name and zone of the DNS record backing the UDP
// LoadBalancer endpoint, used to repoint the UDP SRV records at it.
func UDPRecord(m *freeipav1alpha1.FreeIPA, ingressDomain string) (name, zone string, err error) {
	name, zone, err = HostRecord(m, ingressDomain)
	if err != nil {
		return "", "", err
	}
	return name + "-udp", zone, nil
}

// RealmFor returns the Kerberos realm for the instance, defaulting to the
// upper-cased domain when spec.domain is set, otherwise the upper-cased
// default host name.
func RealmFor(m *freeipav1alpha1.FreeIPA, ingressDomain string) string {
	if m.Spec.Realm != "" {
		return m.Spec.Realm
	}
	if m.Spec.Domain != "" {
		return strings.ToUpper(m.Spec.Domain)
	}
	return strings.ToUpper(DefaultHost(m, ingressDomain))
}

// WorkloadImage returns the container image used to run the FreeIPA workload,
// honoring the per-instance override.
func WorkloadImage(m *freeipav1alpha1.FreeIPA) string {
	if m.Spec.Image != "" {
		return m.Spec.Image
	}
	return freeipav1alpha1.DefaultWorkloadImage
}

// StatefulSetName returns the name of the StatefulSet hosting the FreeIPA
// server.
func StatefulSetName(m *freeipav1alpha1.FreeIPA) string {
	return m.Name
}

// ServiceName returns the name of the LoadBalancer service exposing the
// FreeIPA non-HTTP endpoints.
func ServiceName(m *freeipav1alpha1.FreeIPA) string {
	return m.Name + "-service"
}

// UDPServiceName returns the name of the dedicated LoadBalancer service
// exposing the FreeIPA UDP endpoints, when spec.udpService is configured.
func UDPServiceName(m *freeipav1alpha1.FreeIPA) string {
	return ServiceName(m) + "-udp"
}

// WebServiceName returns the name of the ClusterIP service exposing the nginx
// reverse proxy that fronts the FreeIPA web UI.
func WebServiceName(m *freeipav1alpha1.FreeIPA) string {
	return m.Name + "-web"
}

// WebDeploymentName returns the name of the Deployment running the nginx
// reverse proxy that fronts the FreeIPA web UI.
func WebDeploymentName(m *freeipav1alpha1.FreeIPA) string {
	return m.Name + "-web"
}

// WebConfigMapName returns the name of the config map holding the nginx
// configuration of the web proxy.
func WebConfigMapName(m *freeipav1alpha1.FreeIPA) string {
	return m.Name + "-web-config"
}

// WebConfigHashAnnotation is the pod template annotation holding a hash of the
// nginx configuration, used to roll the web proxy Deployment when the config
// changes (config map volumes mounted with subPath are not refreshed).
const WebConfigHashAnnotation = "freeipa.mathianasj.io/web-config-hash"

// webLabels returns the labels selecting the web proxy resources.
func webLabels(m *freeipav1alpha1.FreeIPA) map[string]string {
	l := LabelsForFreeIPA(m)
	l["component"] = "web"
	return l
}

// servicePorts returns every endpoint the FreeIPA server exposes.
func servicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "http-tcp", Port: 80, TargetPort: intstr.FromString("http-tcp")},
		{Name: "https-tcp", Port: 443, TargetPort: intstr.FromString("https-tcp")},
		{Name: "ldap-tcp", Port: 389, TargetPort: intstr.FromInt(389)},
		{Name: "ldaps-tcp", Port: 636, TargetPort: intstr.FromInt(636)},
		{Name: "kerberos-tcp", Port: 88, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(88)},
		{Name: "kerberos-udp", Port: 88, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt(88)},
		{Name: "kpasswd-tcp", Port: 464, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(464)},
		{Name: "kpasswd-udp", Port: 464, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt(464)},
		{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(53)},
		{Name: "dns-udp", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt(53)},
		{Name: "kadmin-tcp", Port: 749, TargetPort: intstr.FromInt(749)},
	}
}

func tcpPorts(ports []corev1.ServicePort) []corev1.ServicePort {
	var out []corev1.ServicePort
	for _, p := range ports {
		if p.Protocol == corev1.ProtocolUDP {
			continue
		}
		out = append(out, p)
	}
	return out
}

func udpPorts(ports []corev1.ServicePort) []corev1.ServicePort {
	var out []corev1.ServicePort
	for _, p := range ports {
		if p.Protocol != corev1.ProtocolUDP {
			continue
		}
		out = append(out, p)
	}
	return out
}

// RouteName returns the name of the route exposing the FreeIPA web UI.
func RouteName(m *freeipav1alpha1.FreeIPA) string {
	return m.Name
}

// HostnameConfigMapName returns the name of the config map that pins
// /etc/hostname to the instance FQDN.
func HostnameConfigMapName(m *freeipav1alpha1.FreeIPA) string {
	return m.Name + "-hostname"
}

// SecretName returns the name of the secret holding the FreeIPA passwords.
func SecretName(m *freeipav1alpha1.FreeIPA) string {
	if m.Spec.PasswordSecret != nil {
		return *m.Spec.PasswordSecret
	}
	return m.Name + "-passwords"
}

// SCCName returns the name of the SecurityContextConstraints used by the
// FreeIPA workload. SCCs are cluster-scoped so the name incorporates the
// instance namespace to keep it unique.
func SCCName(m *freeipav1alpha1.FreeIPA) string {
	name := "freeipa-" + m.Namespace + "-" + m.Name
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimSuffix(name, "-")
}

// SecretForFreeIPA returns the secret that stores the FreeIPA administrator
// and directory manager passwords.
func SecretForFreeIPA(m *freeipav1alpha1.FreeIPA, adminPassword, dmPassword string) *corev1.Secret {
	if adminPassword == "" {
		adminPassword = GenerateRandomPassword()
	}
	if dmPassword == "" {
		dmPassword = GenerateRandomPassword()
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(m),
			Namespace: m.Namespace,
			Labels:    LabelsForFreeIPA(m),
		},
		Immutable: ptr.To(true),
		Type:      corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"IPA_ADMIN_PASSWORD": adminPassword,
			"IPA_DM_PASSWORD":    dmPassword,
		},
	}
}

// ServiceForFreeIPA returns the LoadBalancer service exposing every FreeIPA
// endpoint that cannot be routed through an Ingress/Route (DNS, Kerberos,
// kpasswd, LDAP/LDAPS and kadmin). When spec.udpService is configured the
// service only carries the TCP endpoints and the UDP ones move to a dedicated
// service.
func ServiceForFreeIPA(m *freeipav1alpha1.FreeIPA) *corev1.Service {
	ports := servicePorts()
	if m.Spec.UDPService != nil {
		ports = tcpPorts(ports)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ServiceName(m),
			Namespace:   m.Namespace,
			Labels:      LabelsForFreeIPA(m),
			Annotations: m.Spec.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: LabelsForFreeIPA(m),
			Ports:    ports,
		},
	}
	if m.Spec.LoadBalancerIP != nil {
		svc.Spec.LoadBalancerIP = *m.Spec.LoadBalancerIP
	}
	return svc
}

// AWSLoadBalancerTypeAnnotation selects the load balancer type on AWS. UDP
// LoadBalancer services are only supported on AWS through a Network Load
// Balancer, so the dedicated UDP service defaults to it.
const AWSLoadBalancerTypeAnnotation = "service.beta.kubernetes.io/aws-load-balancer-type"

// UDPServiceForFreeIPA returns the dedicated LoadBalancer service exposing the
// FreeIPA UDP endpoints (DNS 53, Kerberos 88, kpasswd 464) when
// spec.udpService is configured. UDP LoadBalancer services require a Network
// Load Balancer on AWS (classic ELBs do not support UDP), so the service is
// annotated to request one unless the user overrides it.
func UDPServiceForFreeIPA(m *freeipav1alpha1.FreeIPA) *corev1.Service {
	annotations := maps.Clone(m.Spec.UDPService.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	if _, ok := annotations[AWSLoadBalancerTypeAnnotation]; !ok {
		annotations[AWSLoadBalancerTypeAnnotation] = "nlb"
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        UDPServiceName(m),
			Namespace:   m.Namespace,
			Labels:      LabelsForFreeIPA(m),
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: LabelsForFreeIPA(m),
			Ports:    udpPorts(servicePorts()),
		},
	}
	if m.Spec.UDPService.LoadBalancerIP != nil {
		svc.Spec.LoadBalancerIP = *m.Spec.UDPService.LoadBalancerIP
	}
	return svc
}

// RouteForFreeIPA returns the route exposing the FreeIPA web UI. By default
// the route uses passthrough TLS termination and FreeIPA serves its own
// certificate. When spec.route.termination is reencrypt the route terminates
// TLS at the Ingress Controller (presenting the cluster's default ingress
// certificate) and re-encrypts to FreeIPA, validating the pod certificate
// against destinationCACertificate. When viaWeb is true the route terminates
// TLS at the edge and forwards plain HTTP to the nginx web proxy instead. The
// route host is left empty so that OpenShift generates it from the cluster's
// wildcard *.apps domain.
func RouteForFreeIPA(m *freeipav1alpha1.FreeIPA, destinationCACertificate string, viaWeb bool) *routev1.Route {
	targetService := ServiceName(m)
	targetPort := intstr.FromString("https-tcp")
	tls := &routev1.TLSConfig{
		Termination: routev1.TLSTerminationPassthrough,
	}
	if viaWeb {
		targetService = WebServiceName(m)
		targetPort = intstr.FromString("http")
		tls = &routev1.TLSConfig{
			Termination:                   routev1.TLSTerminationEdge,
			InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
		}
	} else if m.Spec.Route != nil && m.Spec.Route.Termination == string(routev1.TLSTerminationReencrypt) {
		tls = &routev1.TLSConfig{
			Termination:                   routev1.TLSTerminationReencrypt,
			DestinationCACertificate:      destinationCACertificate,
			InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
		}
	}
	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RouteName(m),
			Namespace: m.Namespace,
			Annotations: map[string]string{
				"haproxy.router.openshift.io/timeout":     "2s",
				"haproxy.router.openshift.io/hsts_header": "max-age=31536000;includeSubDomains;preload",
			},
			Labels: LabelsForFreeIPA(m),
		},
		Spec: routev1.RouteSpec{
			Port: &routev1.RoutePort{
				TargetPort: targetPort,
			},
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   targetService,
				Weight: ptr.To(int32(100)),
			},
			TLS: tls,
		},
	}
	return route
}

// HostnameConfigMapForFreeIPA returns the config map whose "hostname" key
// pins the container's /etc/hostname to the instance FQDN. FreeIPA's installer
// calls `hostnamectl set-hostname` with the FQDN; when /etc/hostname already
// contains that value systemd-hostnamed short-circuits and never rewrites the
// file, which on Kubernetes is a bind mount that cannot be atomically
// replaced.
func HostnameConfigMapForFreeIPA(m *freeipav1alpha1.FreeIPA, host string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HostnameConfigMapName(m),
			Namespace: m.Namespace,
			Labels:    LabelsForFreeIPA(m),
		},
		Data: map[string]string{
			"hostname": host + "\n",
		},
	}
}

// WebBackend returns the in-cluster address of the FreeIPA HTTPS endpoint the
// web proxy forwards requests to.
func WebBackend(m *freeipav1alpha1.FreeIPA) string {
	return ServiceName(m) + "." + m.Namespace + ".svc.cluster.local:443"
}

// WebConfigForFreeIPA returns the nginx configuration of the web proxy. nginx
// terminates no TLS itself: it sits behind the edge-terminated route and
// proxies plain HTTP requests to the FreeIPA HTTPS endpoint. It overrides the
// Host header with the instance FQDN so FreeIPA's httpd serves its canonical
// virtual host, and overrides the Referer with the instance FQDN because
// FreeIPA's JSON RPC layer rejects requests whose Referer hostname differs
// from the server hostname. Redirects emitted by FreeIPA pointing at the
// instance FQDN are rewritten to the public route host.
func WebConfigForFreeIPA(m *freeipav1alpha1.FreeIPA, host, backend string) string {
	return fmt.Sprintf(`server {
    listen 8080;
    server_name _;

    location / {
        proxy_pass https://%s;
        proxy_set_header Host %s;
        proxy_set_header Referer https://%s/ipa/ui/;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_ssl_verify off;
        proxy_redirect https://%s/ https://$http_host/;
        proxy_redirect http://%s/ https://$http_host/;
    }
}
`, backend, host, host, host, host)
}

// WebConfigMapForFreeIPA returns the config map holding the nginx
// configuration of the web proxy.
func WebConfigMapForFreeIPA(m *freeipav1alpha1.FreeIPA, host, backend string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WebConfigMapName(m),
			Namespace: m.Namespace,
			Labels:    LabelsForFreeIPA(m),
		},
		Data: map[string]string{
			"default.conf": WebConfigForFreeIPA(m, host, backend),
		},
	}
}

// WebDeploymentForFreeIPA returns the Deployment running the nginx reverse
// proxy in front of the FreeIPA web UI. The config map is mounted with subPath,
// which is not refreshed when the config map changes, so the running config is
// hashed into the pod template to force a rollout whenever it changes.
func WebDeploymentForFreeIPA(m *freeipav1alpha1.FreeIPA, conf string) *appsv1.Deployment {
	labels := webLabels(m)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WebDeploymentName(m),
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						WebConfigHashAnnotation: fmt.Sprintf("%x", sha256.Sum256([]byte(conf))),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: freeipav1alpha1.DefaultWebImage,
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 8080},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(8080),
									},
								},
								PeriodSeconds: 5,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "nginx-config",
									MountPath: "/etc/nginx/conf.d/default.conf",
									SubPath:   "default.conf",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "nginx-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: WebConfigMapName(m)},
								},
							},
						},
					},
				},
			},
		},
	}
}

// WebServiceForFreeIPA returns the ClusterIP service exposing the nginx
// reverse proxy that fronts the FreeIPA web UI.
func WebServiceForFreeIPA(m *freeipav1alpha1.FreeIPA) *corev1.Service {
	labels := webLabels(m)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WebServiceName(m),
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromInt(8080)},
			},
		},
	}
}

// SCCForFreeIPA returns the SecurityContextConstraints required by the
// systemd-based FreeIPA workload.
func SCCForFreeIPA(m *freeipav1alpha1.FreeIPA) *securityv1.SecurityContextConstraints {
	return &securityv1.SecurityContextConstraints{
		ObjectMeta: metav1.ObjectMeta{
			Name: SCCName(m),
			Annotations: map[string]string{
				"kubernetes.io/description":      "Allows the FreeIPA systemd-based workload to run",
				"freeipa.mathianasj.io/owned-by": fmt.Sprintf("%s/%s", m.Namespace, m.Name),
			},
		},
		AllowHostDirVolumePlugin: false,
		AllowHostIPC:             false,
		AllowHostNetwork:         false,
		AllowHostPID:             false,
		AllowHostPorts:           false,
		AllowPrivilegeEscalation: ptr.To(true),
		AllowPrivilegedContainer: true,
		AllowedCapabilities: []corev1.Capability{
			"SETUID", "SETGID", "FSETID", "SETPCAP", "DAC_OVERRIDE",
			"NET_RAW", "NET_BIND_SERVICE", "SYS_CHROOT", "KILL", "AUDIT_WRITE",
			"CHOWN", "FOWNER", "SETFCAP", "SYS_ADMIN", "SYS_RESOURCE", "MKNOD",
		},
		DefaultAddCapabilities: []corev1.Capability{
			"CHOWN", "FOWNER", "SETFCAP", "SETPCAP", "SETUID", "SETGID",
			"DAC_OVERRIDE", "NET_BIND_SERVICE", "KILL", "SYS_ADMIN",
			"SYS_RESOURCE", "MKNOD",
		},
		FSGroup: securityv1.FSGroupStrategyOptions{
			Type: securityv1.FSGroupStrategyRunAsAny,
		},
		RunAsUser: securityv1.RunAsUserStrategyOptions{
			Type: securityv1.RunAsUserStrategyRunAsAny,
		},
		SELinuxContext: securityv1.SELinuxContextStrategyOptions{
			Type: securityv1.SELinuxStrategyMustRunAs,
		},
		SupplementalGroups: securityv1.SupplementalGroupsStrategyOptions{
			Type: securityv1.SupplementalGroupsStrategyRunAsAny,
		},
		Users: []string{
			"system:serviceaccount:" + m.Namespace + ":default",
		},
		Volumes: []securityv1.FSType{
			securityv1.FSTypeConfigMap,
			securityv1.FSTypeDownwardAPI,
			securityv1.FSTypeEmptyDir,
			securityv1.FSTypePersistentVolumeClaim,
			securityv1.FSProjected,
			securityv1.FSTypeSecret,
		},
		Priority: ptr.To(int32(20)),
	}
}

// installArgs returns the arguments passed to the upstream init script, which
// forwards them to ipa-server-install on the first boot.
func installArgs(m *freeipav1alpha1.FreeIPA, host, realm string) []string {
	args := []string{
		"no-exit",
		"ipa-server-install",
		"-U",
		"--hostname", host,
		"--realm", realm,
		"--no-ntp",
		"--no-sshd",
		"--no-ssh",
	}
	if m.Spec.DNS != nil {
		args = append(args, "--setup-dns", "--dns=127.0.0.1")
		if len(m.Spec.DNS.Forwarders) == 0 {
			args = append(args, "--no-forwarders")
		} else {
			for _, f := range m.Spec.DNS.Forwarders {
				args = append(args, "--forwarder", f)
			}
		}
	}
	return args
}

// StatefulSetForFreeIPA returns the StatefulSet running the FreeIPA server.
func StatefulSetForFreeIPA(m *freeipav1alpha1.FreeIPA, ingressDomain string, lbIP string) *appsv1.StatefulSet {
	labels := LabelsForFreeIPA(m)
	host := HostForFreeIPA(m, ingressDomain)

	env := []corev1.EnvVar{
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
		{
			Name: "PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(m)},
					Key:                  "IPA_ADMIN_PASSWORD",
				},
			},
		},
	}
	if lbIP != "" && net.ParseIP(lbIP) != nil {
		env = append(env, corev1.EnvVar{Name: "IPA_SERVER_IP", Value: lbIP})
	}

	volumes := []corev1.Volume{
		{Name: "systemd-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		{Name: "systemd-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		{Name: "hostname", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: HostnameConfigMapName(m)}}}},
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "data", MountPath: "/data"},
		{Name: "systemd-tmp", MountPath: "/tmp"},
		{Name: "systemd-run", MountPath: "/run"},
		{Name: "hostname", MountPath: "/etc/hostname", SubPath: "hostname"},
	}

	var volumeClaimTemplates []corev1.PersistentVolumeClaim
	if m.Spec.VolumeClaimTemplate != nil {
		volumeClaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "data",
				Labels: labels,
			},
			Spec: *m.Spec.VolumeClaimTemplate,
		}}
	} else {
		volumes = append(volumes, corev1.Volume{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      StatefulSetName(m),
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			ServiceName: ServiceName(m),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"openshift.io/scc": SCCName(m)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:      "freeipa",
							Image:     WorkloadImage(m),
							TTY:       true,
							Resources: m.Spec.Resources,
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{
										"NET_RAW", "SYS_CHROOT", "AUDIT_CONTROL", "AUDIT_READ",
										"BLOCK_SUSPEND", "DAC_READ_SEARCH", "IPC_LOCK", "IPC_OWNER",
										"LEASE", "LINUX_IMMUTABLE", "MAC_ADMIN", "MAC_OVERRIDE",
										"NET_ADMIN", "NET_BROADCAST", "SYS_BOOT", "SYS_MODULE",
										"SYS_NICE", "SYS_PACCT", "SYS_PTRACE", "SYS_RAWIO",
										"SYS_TIME", "SYS_TTY_CONFIG", "SYSLOG", "WAKE_ALARM",
									},
									Add: []corev1.Capability{
										"CHOWN", "FOWNER", "DAC_OVERRIDE", "SETUID", "SETGID",
										"KILL", "NET_BIND_SERVICE", "SETPCAP", "SETFCAP",
										"SYS_ADMIN", "SYS_RESOURCE", "FSETID", "MKNOD",
									},
								},
							},
							Command: []string{
								"/bin/sh", "-c",
								fmt.Sprintf("if ! grep -q '%s' /etc/hosts; then tmp=$(mktemp) && { echo \"$POD_IP %s %s.\"; cat /etc/hosts; } > \"$tmp\" && cat \"$tmp\" > /etc/hosts && rm -f \"$tmp\"; fi; exec /usr/local/sbin/init \"$@\"", host, host, host),
								"init",
							},
							Args: installArgs(m, host, RealmFor(m, ingressDomain)),
							EnvFrom: []corev1.EnvFromSource{
								{
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(m)},
									},
								},
							},
							Env: env,
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"kill", "-RTMIN+3", "1"},
									},
								},
							},
							Ports: []corev1.ContainerPort{
								{Name: "http-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 80},
								{Name: "https-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 443},
								{Name: "ldap-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 389},
								{Name: "ldaps-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 636},
								{Name: "kerberos-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 88},
								{Name: "kerberos-udp", Protocol: corev1.ProtocolUDP, ContainerPort: 88},
								{Name: "kpasswd-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 464},
								{Name: "kpasswd-udp", Protocol: corev1.ProtocolUDP, ContainerPort: 464},
								{Name: "dns-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 53},
								{Name: "dns-udp", Protocol: corev1.ProtocolUDP, ContainerPort: 53},
								{Name: "kadmin-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: 749},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/usr/bin/systemctl", "status", "ipa"},
									},
								},
								InitialDelaySeconds: 60,
								TimeoutSeconds:      10,
								PeriodSeconds:       10,
								SuccessThreshold:    1,
								FailureThreshold:    3,
							},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates,
		},
	}
	return sts
}
