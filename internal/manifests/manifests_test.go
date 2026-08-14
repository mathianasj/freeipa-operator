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

package manifests

import (
	"reflect"
	"strings"
	"testing"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	freeipav1alpha1 "github.com/mathianasj/freeipa-operator/api/v1alpha1"
)

func instance(name, namespace string) *freeipav1alpha1.FreeIPA {
	return &freeipav1alpha1.FreeIPA{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "11111111-1111-1111-1111-111111111111"},
	}
}

func TestDefaultHostAndRealm(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	host := DefaultHost(m, "apps.example.com")
	if host != "freeipa-sample-ipa.apps.example.com" {
		t.Fatalf("unexpected host: %s", host)
	}
	realm := RealmFor(m, "apps.example.com")
	if realm != "FREEIPA-SAMPLE-IPA.APPS.EXAMPLE.COM" {
		t.Fatalf("unexpected realm: %s", realm)
	}

	m.Spec.Realm = "EXAMPLE.ORG"
	if got := RealmFor(m, "apps.example.com"); got != "EXAMPLE.ORG" {
		t.Fatalf("explicit realm not honored: %s", got)
	}
}

func TestDomainOverride(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.Domain = "example.com"

	// The cluster ingress domain must be ignored when spec.domain is set.
	if got := DefaultHost(m, "apps.example.com"); got != "freeipa-sample-ipa.example.com" {
		t.Fatalf("default host must derive from spec.domain: %s", got)
	}
	if got := HostForFreeIPA(m, "apps.example.com"); got != "freeipa-sample-ipa.example.com" {
		t.Fatalf("host must derive from spec.domain: %s", got)
	}
	// The realm defaults to the upper-cased domain.
	if got := RealmFor(m, "apps.example.com"); got != "EXAMPLE.COM" {
		t.Fatalf("realm must default to the upper-cased domain: %s", got)
	}

	// An explicit host still wins over the domain-derived default.
	m.Spec.Host = "ipa.example.org"
	if got := HostForFreeIPA(m, "apps.example.com"); got != "ipa.example.org" {
		t.Fatalf("explicit host must win over the domain: %s", got)
	}
	// An explicit realm still wins over the domain-derived default.
	m.Spec.Realm = "EXAMPLE.NET"
	if got := RealmFor(m, "apps.example.com"); got != "EXAMPLE.NET" {
		t.Fatalf("explicit realm must win over the domain: %s", got)
	}
}

func TestWorkloadImageDefaultAndOverride(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	if got := WorkloadImage(m); got != freeipav1alpha1.DefaultWorkloadImage {
		t.Fatalf("unexpected default image: %s", got)
	}
	m.Spec.Image = "example.com/custom:1.0"
	if got := WorkloadImage(m); got != "example.com/custom:1.0" {
		t.Fatalf("override not honored: %s", got)
	}
}

func TestSecretForFreeIPA(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	secret := SecretForFreeIPA(m, "admin-pass", "dm-pass")
	if secret.Name != "freeipa-sample-passwords" {
		t.Fatalf("unexpected secret name: %s", secret.Name)
	}
	if secret.StringData["IPA_ADMIN_PASSWORD"] != "admin-pass" {
		t.Fatalf("admin password not set")
	}
	if secret.StringData["IPA_DM_PASSWORD"] != "dm-pass" {
		t.Fatalf("dm password not set")
	}
	if secret.Immutable == nil || !*secret.Immutable {
		t.Fatalf("secret should be immutable")
	}

	m.Spec.PasswordSecret = ptrStr("my-secret")
	if got := SecretName(m); got != "my-secret" {
		t.Fatalf("referenced secret name not honored: %s", got)
	}
}

func TestServiceForFreeIPA(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.LoadBalancerIP = ptrStr("192.168.100.10")
	svc := ServiceForFreeIPA(m)

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("service must be LoadBalancer, got %s", svc.Spec.Type)
	}
	if svc.Spec.LoadBalancerIP != "192.168.100.10" {
		t.Fatalf("loadBalancerIP not honored")
	}
	wantPorts := map[string]int32{
		"http-tcp": 80, "https-tcp": 443, "ldap-tcp": 389, "ldaps-tcp": 636,
		"kerberos-tcp": 88, "kerberos-udp": 88, "kpasswd-tcp": 464, "kpasswd-udp": 464,
		"dns-tcp": 53, "dns-udp": 53, "kadmin-tcp": 749,
	}
	if len(svc.Spec.Ports) != len(wantPorts) {
		t.Fatalf("unexpected port count: %d", len(svc.Spec.Ports))
	}
	for _, p := range svc.Spec.Ports {
		if want, ok := wantPorts[p.Name]; !ok || want != p.Port {
			t.Fatalf("unexpected port %s=%d", p.Name, p.Port)
		}
	}
}

func TestUDPServiceSplit(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.UDPService = &freeipav1alpha1.UDPServiceSpec{
		LoadBalancerIP: ptrStr("192.168.100.11"),
	}

	svc := ServiceForFreeIPA(m)
	if svc.Name != ServiceName(m) {
		t.Fatalf("unexpected service name: %s", svc.Name)
	}
	for _, p := range svc.Spec.Ports {
		if p.Protocol == corev1.ProtocolUDP {
			t.Fatalf("main service must not expose UDP ports when udpService is set: %s", p.Name)
		}
	}

	udpSvc := UDPServiceForFreeIPA(m)
	if udpSvc.Name != UDPServiceName(m) {
		t.Fatalf("unexpected udp service name: %s", udpSvc.Name)
	}
	if udpSvc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("udp service must be LoadBalancer")
	}
	if udpSvc.Spec.LoadBalancerIP != "192.168.100.11" {
		t.Fatalf("udp loadBalancerIP not honored")
	}
	if udpSvc.Annotations[AWSLoadBalancerTypeAnnotation] != "nlb" {
		t.Fatalf("udp service must default to an AWS NLB, got %q", udpSvc.Annotations[AWSLoadBalancerTypeAnnotation])
	}
	wantUDP := map[string]int32{"kerberos-udp": 88, "kpasswd-udp": 464, "dns-udp": 53}
	if len(udpSvc.Spec.Ports) != len(wantUDP) {
		t.Fatalf("unexpected udp port count: %d", len(udpSvc.Spec.Ports))
	}
	for _, p := range udpSvc.Spec.Ports {
		if want, ok := wantUDP[p.Name]; !ok || want != p.Port {
			t.Fatalf("unexpected udp port %s=%d", p.Name, p.Port)
		}
	}

	override := instance("freeipa-sample", "ipa")
	override.Spec.UDPService = &freeipav1alpha1.UDPServiceSpec{
		Annotations: map[string]string{AWSLoadBalancerTypeAnnotation: "clb"},
	}
	if got := UDPServiceForFreeIPA(override).Annotations[AWSLoadBalancerTypeAnnotation]; got != "clb" {
		t.Fatalf("user annotation must not be overridden, got %q", got)
	}
}

func TestStatefulSetForFreeIPA(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	sts := StatefulSetForFreeIPA(m, "apps.example.com", "192.168.100.10")

	if sts.Spec.Template.Spec.Containers[0].Image != freeipav1alpha1.DefaultWorkloadImage {
		t.Fatalf("unexpected container image")
	}
	gotEnv := map[string]string{}
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		gotEnv[e.Name] = e.Value
	}
	if sts.Spec.Template.Spec.Hostname != "" {
		t.Fatalf("pod hostname must be left for the StatefulSet controller")
	}
	if gotEnv["IPA_SERVER_IP"] != "192.168.100.10" {
		t.Fatalf("unexpected IPA_SERVER_IP env: %s", gotEnv["IPA_SERVER_IP"])
	}

	// The command must prepend the FQDN to /etc/hosts (before the kubelet
	// entries) so both forward resolution and reverse-DNS of the pod IP
	// return the FQDN; httpd derives its ServerName from the reverse lookup
	// and mod_ssl keys its passphrase lookup on that name.
	cmd := sts.Spec.Template.Spec.Containers[0].Command
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "exec /usr/local/sbin/init") {
		t.Fatalf("command must exec the upstream init script: %v", cmd)
	}
	if !strings.Contains(joined, "freeipa-sample-ipa.apps.example.com") {
		t.Fatalf("command must pin the FQDN into /etc/hosts: %v", cmd)
	}
	if !strings.Contains(joined, "cat /etc/hosts") {
		t.Fatalf("command must insert the FQDN before the kubelet hosts entries: %v", cmd)
	}
	var podIPEnv *corev1.EnvVar
	for i := range sts.Spec.Template.Spec.Containers[0].Env {
		e := &sts.Spec.Template.Spec.Containers[0].Env[i]
		if e.Name == "POD_IP" {
			podIPEnv = e
		}
	}
	if podIPEnv == nil || podIPEnv.ValueFrom == nil || podIPEnv.ValueFrom.FieldRef == nil ||
		podIPEnv.ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Fatalf("POD_IP env must come from the downward API")
	}

	// /etc/hostname must be pinned to the FQDN via a config map so that the
	// installer's hostnamectl set-hostname call is a no-op.
	var hostnameMount *corev1.VolumeMount
	for i := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if sts.Spec.Template.Spec.Containers[0].VolumeMounts[i].MountPath == "/etc/hostname" {
			hostnameMount = &sts.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if hostnameMount == nil {
		t.Fatalf("missing /etc/hostname volume mount")
	}
	if hostnameMount.SubPath != "hostname" {
		t.Fatalf("unexpected /etc/hostname subPath: %s", hostnameMount.SubPath)
	}
	var hostnameVolume *corev1.Volume
	for i := range sts.Spec.Template.Spec.Volumes {
		if sts.Spec.Template.Spec.Volumes[i].Name == hostnameMount.Name {
			hostnameVolume = &sts.Spec.Template.Spec.Volumes[i]
		}
	}
	if hostnameVolume == nil || hostnameVolume.ConfigMap == nil || hostnameVolume.ConfigMap.Name != HostnameConfigMapName(m) {
		t.Fatalf("missing hostname config map volume")
	}

	if sts.Spec.Template.Spec.Containers[0].ReadinessProbe == nil {
		t.Fatalf("readiness probe missing")
	}
	sec := sts.Spec.Template.Spec.Containers[0].SecurityContext
	if sec == nil || sec.Privileged == nil || !*sec.Privileged {
		t.Fatalf("freeipa container must run privileged for systemd")
	}
	if len(sts.Spec.VolumeClaimTemplates) != 0 {
		t.Fatalf("no volumeClaimTemplate should be generated when spec is empty")
	}

	// The install command must pin the FQDN and realm.
	args := sts.Spec.Template.Spec.Containers[0].Args
	if !strings.Contains(strings.Join(args, " "), "--hostname freeipa-sample-ipa.apps.example.com") {
		t.Fatalf("install command must pin --hostname: %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "--setup-dns") || strings.Contains(strings.Join(args, " "), "--setup-pkinit") {
		t.Fatalf("DNS and PKINIT must be off by default: %v", args)
	}

	// A second render must produce the same pod template (no churn).
	sts2 := StatefulSetForFreeIPA(m, "apps.example.com", "192.168.100.10")
	if !reflect.DeepEqual(sts.Spec.Template, sts2.Spec.Template) {
		t.Fatalf("pod template is not stable across renders")
	}
}

func TestStatefulSetDNS(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.DNS = &freeipav1alpha1.DNSSpec{Forwarders: []string{"1.1.1.1", "8.8.8.8"}}

	sts := StatefulSetForFreeIPA(m, "apps.example.com", "")
	args := sts.Spec.Template.Spec.Containers[0].Args
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--setup-dns") {
		t.Fatalf("expected --setup-dns: %v", args)
	}
	for _, f := range []string{"--forwarder 1.1.1.1", "--forwarder 8.8.8.8"} {
		if !strings.Contains(joined, f) {
			t.Fatalf("expected %q: %v", f, args)
		}
	}
	if strings.Contains(joined, "--no-forwarders") {
		t.Fatalf("--no-forwarders must not be set when forwarders are given: %v", args)
	}
}

func TestStatefulSetDNSNoForwarders(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.DNS = &freeipav1alpha1.DNSSpec{}

	sts := StatefulSetForFreeIPA(m, "apps.example.com", "")
	args := sts.Spec.Template.Spec.Containers[0].Args
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--setup-dns") || !strings.Contains(joined, "--no-forwarders") {
		t.Fatalf("expected --setup-dns --no-forwarders: %v", args)
	}
}

func TestIPAServerIPRequiresLiteralAddress(t *testing.T) {
	// A load balancer that yields a hostname (e.g. AWS NLB) must not be
	// written to IPA_SERVER_IP; the installer only accepts literal IPs there
	// and the DNS self-update path would break.
	m := instance("freeipa-sample", "ipa")
	sts := StatefulSetForFreeIPA(m, "apps.example.com", "a21fb6398ae284c97be0ead9c6a3ba57.elb.amazonaws.com")
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "IPA_SERVER_IP" {
			t.Fatalf("IPA_SERVER_IP must not be set from a hostname")
		}
	}

	sts2 := StatefulSetForFreeIPA(m, "apps.example.com", "192.168.100.10")
	found := false
	for _, e := range sts2.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "IPA_SERVER_IP" && e.Value == "192.168.100.10" {
			found = true
		}
	}
	if !found {
		t.Fatalf("IPA_SERVER_IP must be set from a literal IP")
	}
}

func TestStatefulSetWithVolumeClaimTemplate(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.VolumeClaimTemplate = &corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}
	sts := StatefulSetForFreeIPA(m, "apps.example.com", "")
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected one volumeClaimTemplate")
	}
	if sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Fatalf("expected volumeClaimTemplate named data")
	}
}

func TestHostnameConfigMapForFreeIPA(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	cm := HostnameConfigMapForFreeIPA(m, "freeipa-sample-ipa.apps.example.com")
	if cm.Name != HostnameConfigMapName(m) {
		t.Fatalf("unexpected config map name: %s", cm.Name)
	}
	if got := cm.Data["hostname"]; got != "freeipa-sample-ipa.apps.example.com\n" {
		t.Fatalf("unexpected hostname data: %q", got)
	}
}

func TestRouteForFreeIPA(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	route := RouteForFreeIPA(m, "", false)
	if route.Spec.Host != "" {
		t.Fatalf("route host must be left for OpenShift to generate, got %s", route.Spec.Host)
	}
	if _, ok := route.Annotations["openshift.io/host.generated"]; ok {
		t.Fatalf("host.generated annotation must not be set by the operator")
	}
	if route.Spec.TLS == nil || route.Spec.TLS.Termination != routev1.TLSTerminationPassthrough {
		t.Fatalf("route must use passthrough TLS by default")
	}
	if route.Spec.To.Name != ServiceName(m) {
		t.Fatalf("route must target the LoadBalancer service")
	}
	if route.Spec.Port == nil || route.Spec.Port.TargetPort.String() != "https-tcp" {
		t.Fatalf("route must target the https-tcp port")
	}
}

func TestRouteForFreeIPAReencrypt(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.Route = &freeipav1alpha1.RouteSpec{Termination: "reencrypt"}
	const ca = "-----BEGIN CERTIFICATE-----\nMIICPQ==\n-----END CERTIFICATE-----\n"
	route := RouteForFreeIPA(m, ca, false)
	if route.Spec.Host != "" {
		t.Fatalf("route host must be left for OpenShift to generate, got %s", route.Spec.Host)
	}
	if route.Spec.TLS == nil || route.Spec.TLS.Termination != routev1.TLSTerminationReencrypt {
		t.Fatalf("route must use reencrypt TLS, got %v", route.Spec.TLS)
	}
	if route.Spec.TLS.DestinationCACertificate != ca {
		t.Fatalf("destination CA certificate not propagated to the route")
	}
	if route.Spec.TLS.InsecureEdgeTerminationPolicy != routev1.InsecureEdgeTerminationPolicyRedirect {
		t.Fatalf("insecure edge termination policy must be Redirect, got %s", route.Spec.TLS.InsecureEdgeTerminationPolicy)
	}
	if route.Spec.TLS.Certificate != "" || route.Spec.TLS.Key != "" {
		t.Fatalf("route must present the cluster default certificate, not an operator-provided one")
	}
	if route.Spec.To.Name != ServiceName(m) {
		t.Fatalf("route must target the LoadBalancer service")
	}
}

func TestRouteForFreeIPAViaWeb(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	route := RouteForFreeIPA(m, "ignored-ca", true)
	if route.Spec.TLS == nil || route.Spec.TLS.Termination != routev1.TLSTerminationEdge {
		t.Fatalf("route must use edge TLS when fronted by the web proxy, got %v", route.Spec.TLS)
	}
	if route.Spec.TLS.DestinationCACertificate != "" {
		t.Fatalf("route must not carry a destination CA when fronted by the web proxy")
	}
	if route.Spec.TLS.InsecureEdgeTerminationPolicy != routev1.InsecureEdgeTerminationPolicyRedirect {
		t.Fatalf("insecure edge termination policy must be Redirect, got %s", route.Spec.TLS.InsecureEdgeTerminationPolicy)
	}
	if route.Spec.To.Name != WebServiceName(m) {
		t.Fatalf("route must target the web proxy service, got %s", route.Spec.To.Name)
	}
	if route.Spec.Port == nil || route.Spec.Port.TargetPort.String() != "http" {
		t.Fatalf("route must target the http port of the web proxy")
	}
}

func TestRouteHost(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	if got := RouteHost(m, "apps.example.com"); got != "freeipa-sample-ipa.apps.example.com" {
		t.Fatalf("unexpected route host: %s", got)
	}
	m.Spec.Domain = "example.test"
	if got := RouteHost(m, "apps.example.com"); got != "freeipa-sample-ipa.apps.example.com" {
		t.Fatalf("route host must ignore spec.domain, got %s", got)
	}
}

func TestHostRecord(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.Domain = "example.test"
	name, zone, err := HostRecord(m, "apps.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if name != "freeipa-sample-ipa" || zone != "example.test" {
		t.Fatalf("unexpected record name/zone: %s/%s", name, zone)
	}

	m.Spec.Host = "ipa.example.org"
	if _, _, err := HostRecord(m, "apps.example.com"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	m.Spec.Host = "singlelabel"
	if _, _, err := HostRecord(m, "apps.example.com"); err == nil {
		t.Fatalf("expected an error for a host without a zone")
	}
}

func TestUDPRecord(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.Domain = "example.test"
	name, zone, err := UDPRecord(m, "apps.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if name != "freeipa-sample-ipa-udp" || zone != "example.test" {
		t.Fatalf("unexpected udp record name/zone: %s/%s", name, zone)
	}
}

func TestWebProxyManifests(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	m.Spec.Domain = "example.test"

	cm := WebConfigMapForFreeIPA(m, "freeipa-sample-ipa.example.test", WebBackend(m))
	if cm.Name != WebConfigMapName(m) {
		t.Fatalf("unexpected config map name: %s", cm.Name)
	}
	conf, ok := cm.Data["default.conf"]
	if !ok {
		t.Fatalf("config map must contain a default.conf key")
	}
	for _, want := range []string{
		"listen 8080",
		"proxy_pass https://" + WebBackend(m),
		"proxy_set_header Host freeipa-sample-ipa.example.test;",
		"proxy_set_header Referer https://freeipa-sample-ipa.example.test/ipa/ui/;",
		"proxy_ssl_verify off;",
		"proxy_redirect https://freeipa-sample-ipa.example.test/ https://$http_host/;",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("nginx config must contain %q, got:\n%s", want, conf)
		}
	}

	dep := WebDeploymentForFreeIPA(m, conf)
	if dep.Name != WebDeploymentName(m) {
		t.Fatalf("unexpected deployment name: %s", dep.Name)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Fatalf("web proxy must run a single replica")
	}
	if dep.Spec.Template.Annotations[WebConfigHashAnnotation] == "" {
		t.Fatalf("deployment must carry the web config hash annotation")
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != freeipav1alpha1.DefaultWebImage {
		t.Fatalf("unexpected nginx image: %s", c.Image)
	}
	if c.Ports[0].ContainerPort != 8080 {
		t.Fatalf("nginx must listen on 8080")
	}
	if c.VolumeMounts[0].MountPath != "/etc/nginx/conf.d/default.conf" {
		t.Fatalf("nginx config must be mounted over default.conf")
	}
	if dep.Spec.Template.Spec.Volumes[0].ConfigMap.Name != WebConfigMapName(m) {
		t.Fatalf("deployment must reference the web proxy config map")
	}

	svc := WebServiceForFreeIPA(m)
	if svc.Name != WebServiceName(m) {
		t.Fatalf("unexpected service name: %s", svc.Name)
	}
	if svc.Spec.Ports[0].Port != 8080 {
		t.Fatalf("web proxy service must expose 8080")
	}
	if !reflect.DeepEqual(svc.Spec.Selector, webLabels(m)) {
		t.Fatalf("web proxy service must select the web proxy pods")
	}
}

func TestSCCForFreeIPA(t *testing.T) {
	m := instance("freeipa-sample", "ipa")
	scc := SCCForFreeIPA(m)
	if scc.Name != "freeipa-ipa-freeipa-sample" {
		t.Fatalf("unexpected scc name: %s", scc.Name)
	}
	wantUser := "system:serviceaccount:ipa:default"
	if len(scc.Users) != 1 || scc.Users[0] != wantUser {
		t.Fatalf("unexpected scc users: %v", scc.Users)
	}
	if scc.RunAsUser.Type != "RunAsAny" {
		t.Fatalf("runAsUser must be RunAsAny")
	}
	if !scc.AllowPrivilegedContainer {
		t.Fatalf("the systemd-based workload requires a privileged SCC")
	}
}

func ptrStr(s string) *string {
	return &s
}
