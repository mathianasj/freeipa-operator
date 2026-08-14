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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultWorkloadImage is the default container image used to run the FreeIPA
// server workload. It can be overridden per instance through FreeIPASpec.Image.
const DefaultWorkloadImage = "quay.io/freeipa/freeipa-server:fedora-44"

// DefaultWebImage is the default container image used to run the nginx reverse
// proxy that fronts the FreeIPA web UI when the route hostname differs from
// the instance hostname.
const DefaultWebImage = "nginxinc/nginx-unprivileged:alpine"

// Condition types surfaced on FreeIPAStatus.Conditions.
const (
	// ConditionAvailable is set to True when the FreeIPA workload is deployed
	// and the server reports Ready.
	ConditionAvailable string = "Available"
	// ConditionProgressing is set to True while the operator is still
	// deploying or updating the FreeIPA instance.
	ConditionProgressing string = "Progressing"
	// ConditionDegraded is set to True when the FreeIPA instance is failing
	// and needs attention.
	ConditionDegraded string = "Degraded"
	// ConditionLoadBalancerPending is set to True while waiting for the
	// LoadBalancer service to receive an external IP address.
	ConditionLoadBalancerPending string = "LoadBalancerPending"
)

// Lifecycle phases reported on FreeIPAStatus.Phase.
const (
	// PhasePending the CR has been accepted but no workload exists yet.
	PhasePending string = "Pending"
	// PhaseProvisioning the workload is being deployed.
	PhaseProvisioning string = "Provisioning"
	// PhaseReady the FreeIPA server is deployed and reporting Ready.
	PhaseReady string = "Ready"
	// PhaseDegraded the instance is in a degraded state.
	PhaseDegraded string = "Degraded"
	// PhaseDeleting the instance is being removed.
	PhaseDeleting string = "Deleting"
)

// FreeIPASpec defines the desired state of FreeIPA.
type FreeIPASpec struct {
	// Host is the fully qualified hostname used when installing the FreeIPA
	// server. When omitted it defaults to
	// <instance>.<namespace>.<cluster-ingress-domain>.
	// +kubebuilder:validation:MaxLength:=64
	// +optional
	Host string `json:"host,omitempty"`

	// Domain is the root DNS domain used to derive the default host name and
	// Kerberos realm. When set the default host is
	// <instance>.<namespace>.<domain> and the realm defaults to the
	// upper-cased domain. When omitted the cluster ingress domain is used.
	// +optional
	Domain string `json:"domain,omitempty"`

	// Realm is the Kerberos realm managed by the FreeIPA instance. When
	// omitted it is derived from the host name (upper-cased DNS name).
	// +optional
	Realm string `json:"realm,omitempty"`

	// PasswordSecret references an existing secret holding the FreeIPA
	// administrator and directory manager passwords under the keys
	// IPA_ADMIN_PASSWORD and IPA_DM_PASSWORD. When omitted the operator
	// generates a secret with random passwords.
	// +optional
	PasswordSecret *string `json:"passwordSecret,omitempty"`

	// Resources describe the compute resource requirements of the FreeIPA
	// server container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// VolumeClaimTemplate defines the persistent storage used to keep the
	// FreeIPA server data under /data. When omitted an ephemeral volume is
	// used (data is lost on pod restart).
	// +optional
	VolumeClaimTemplate *corev1.PersistentVolumeClaimSpec `json:"volumeClaimTemplate,omitempty"`

	// Image overrides the container image used to run the FreeIPA server
	// workload. When omitted DefaultWorkloadImage is used.
	// +optional
	Image string `json:"image,omitempty"`

	// LoadBalancerIP requests a specific external IP address for the
	// LoadBalancer service. It must belong to an address pool managed by the
	// load balancer provider (e.g. a MetalLB IPAddressPool).
	// +optional
	LoadBalancerIP *string `json:"loadBalancerIP,omitempty"`

	// ServiceAnnotations are extra annotations applied to the LoadBalancer
	// service exposing the FreeIPA non-HTTP endpoints.
	// +optional
	ServiceAnnotations map[string]string `json:"serviceAnnotations,omitempty"`

	// UDPService optionally exposes the UDP endpoints (DNS 53, Kerberos 88,
	// kpasswd 464) through a dedicated LoadBalancer service. Some providers
	// (e.g. AWS NLB) cannot host TCP and UDP on the same load balancer;
	// when set, the main service only exposes the TCP endpoints and a second
	// service named <instance>-service-udp is created for the UDP ones.
	// +optional
	UDPService *UDPServiceSpec `json:"udpService,omitempty"`

	// DNS configures the integrated DNS server (named + bind-dyndb-ldap).
	// When set, the installer runs with --setup-dns and the pod serves the
	// realm zone authoritatively on the 53/tcp and 53/udp endpoints already
	// exposed by the services.
	// +optional
	DNS *DNSSpec `json:"dns,omitempty"`

	// Route configures the TLS termination of the OpenShift Route exposing
	// the FreeIPA web UI. When omitted the route uses passthrough termination
	// and FreeIPA serves its own certificate.
	// +optional
	Route *RouteSpec `json:"route,omitempty"`
}

// RouteSpec configures the OpenShift Route exposing the FreeIPA web UI.
type RouteSpec struct {
	// Termination is the TLS termination strategy of the route.
	// passthrough (default) lets FreeIPA serve its own certificate directly.
	// reencrypt terminates TLS at the Ingress Controller, which presents the
	// cluster's default ingress certificate, and re-encrypts to FreeIPA.
	// +kubebuilder:validation:Enum=passthrough;reencrypt
	// +kubebuilder:default=passthrough
	// +optional
	Termination string `json:"termination,omitempty"`

	// DestinationCACertificate is the PEM-encoded certificate of the FreeIPA
	// CA that signs the web server certificate. It is used when Termination is
	// reencrypt so the Ingress Controller can validate the certificate
	// presented by the FreeIPA pod. When omitted the operator automatically
	// fetches the CA certificate from the running server.
	// +optional
	DestinationCACertificate string `json:"destinationCACertificate,omitempty"`
}

// DNSSpec configures the integrated DNS server.
type DNSSpec struct {
	// Forwarders are upstream DNS servers used for recursive lookups
	// (passed as --forwarder). When empty, --no-forwarders is used and the
	// server answers authoritatively only.
	// +optional
	Forwarders []string `json:"forwarders,omitempty"`
}

// UDPServiceSpec configures the dedicated LoadBalancer service exposing the
// FreeIPA UDP endpoints.
type UDPServiceSpec struct {
	// LoadBalancerIP requests a specific external IP address for the UDP
	// LoadBalancer service.
	// +optional
	LoadBalancerIP *string `json:"loadBalancerIP,omitempty"`

	// Annotations are extra annotations applied to the UDP LoadBalancer
	// service.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// FreeIPAStatus defines the observed state of FreeIPA.
type FreeIPAStatus struct {
	// SecretName is the name of the secret holding the FreeIPA passwords.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Master is the name of the StatefulSet and pod running the FreeIPA
	// server.
	// +optional
	Master string `json:"master,omitempty"`

	// LoadBalancerIP is the externally reachable IP address allocated for the
	// LoadBalancer service, once available.
	// +optional
	LoadBalancerIP string `json:"loadBalancerIP,omitempty"`

	// UDPLoadBalancerIP is the externally reachable IP address allocated for
	// the UDP LoadBalancer service, once available.
	// +optional
	UDPLoadBalancerIP string `json:"udpLoadBalancerIP,omitempty"`

	// Phase is a coarse-grained lifecycle phase for the instance.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions describe the detailed state of the instance.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=freeipas,scope=Namespaced,shortName=fipa
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Master",type=string,JSONPath=`.status.master`
// +kubebuilder:printcolumn:name="LB IP",type=string,JSONPath=`.status.loadBalancerIP`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FreeIPA is the Schema for the freeipas API.
type FreeIPA struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FreeIPASpec   `json:"spec,omitempty"`
	Status FreeIPAStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FreeIPAList contains a list of FreeIPA.
type FreeIPAList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FreeIPA `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FreeIPA{}, &FreeIPAList{})
}
