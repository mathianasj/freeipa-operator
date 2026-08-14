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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	freeipav1alpha1 "github.com/mathianasj/freeipa-operator/api/v1alpha1"
	"github.com/mathianasj/freeipa-operator/internal/manifests"
)

var _ = Describe("FreeIPA Controller", func() {
	ctx := context.Background()
	var reconciler *FreeIPAReconciler

	BeforeEach(func() {
		reconciler = &FreeIPAReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	Context("When reconciling a resource", func() {
		It("should ignore resources that do not exist", func() {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "missing-resource", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	Context("When handling resource deletion", func() {
		It("should remove the finalizer", func() {
			resource := &freeipav1alpha1.FreeIPA{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "deleting-resource",
					Namespace:  "default",
					Finalizers: []string{finalizerName},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			// Deleting a resource that still has a finalizer sets the
			// DeletionTimestamp without removing the object.
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			got := &freeipav1alpha1.FreeIPA{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "deleting-resource", Namespace: "default"}, got)).To(Succeed())
			Expect(got.DeletionTimestamp).NotTo(BeNil())

			_, err := reconciler.reconcileDelete(ctx, got)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "deleting-resource", Namespace: "default"}, got)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When ensuring the passwords secret", func() {
		It("should create a secret with random passwords", func() {
			resource := &freeipav1alpha1.FreeIPA{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret-resource",
					Namespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(k8sClient.Delete, resource)

			Expect(reconciler.reconcileSecret(ctx, resource)).To(Succeed())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: manifests.SecretName(resource), Namespace: "default"}, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKey("IPA_ADMIN_PASSWORD"))
			Expect(secret.Data).To(HaveKey("IPA_DM_PASSWORD"))
		})
	})

	Context("When comparing route TLS configuration", func() {
		It("should only reconcile the operator-owned TLS fields", func() {
			route := &routev1.Route{
				Spec: routev1.RouteSpec{
					Host: "freeipa-sample-ipa.apps.example.com",
					TLS: &routev1.TLSConfig{
						Termination:              routev1.TLSTerminationPassthrough,
						DestinationCACertificate: "old-ca",
					},
				},
			}
			desired := &routev1.Route{
				Spec: routev1.RouteSpec{
					TLS: &routev1.TLSConfig{
						Termination:                   routev1.TLSTerminationReencrypt,
						DestinationCACertificate:      "new-ca",
						InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
					},
				},
			}
			Expect(routeNeedsUpdate(route, desired)).To(BeTrue())
			desired.Spec.TLS.Termination = routev1.TLSTerminationPassthrough
			desired.Spec.TLS.DestinationCACertificate = "old-ca"
			desired.Spec.TLS.InsecureEdgeTerminationPolicy = ""
			Expect(routeNeedsUpdate(route, desired)).To(BeFalse())
		})
	})

	Context("When the instance pins its own domain", func() {
		It("should not depend on the cluster ingress domain", func() {
			resource := &freeipav1alpha1.FreeIPA{
				ObjectMeta: metav1.ObjectMeta{Name: "domain-resource", Namespace: "default"},
				Spec:       freeipav1alpha1.FreeIPASpec{Domain: "example.com"},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			DeferCleanup(k8sClient.Delete, resource)

			// The web proxy must be active whenever the public route hostname
			// differs from the instance hostname.
			reconciler.IngressDomain = "apps.example.com"
			Expect(reconciler.webProxyActive(resource)).To(BeTrue())
			resource.Spec.Domain = ""
			Expect(reconciler.webProxyActive(resource)).To(BeFalse())
			resource.Spec.Host = "ipa.example.org"
			Expect(reconciler.webProxyActive(resource)).To(BeTrue())

			// The hostname config map is derived from spec.domain even when
			// the reconciler never read a cluster ingress domain.
			resource.Spec.Host = ""
			resource.Spec.Domain = "example.com"
			Expect(reconciler.reconcileHostnameConfigMap(ctx, resource)).To(Succeed())
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      manifests.HostnameConfigMapName(resource),
				Namespace: "default",
			}, cm)).To(Succeed())
			Expect(cm.Data).To(HaveKeyWithValue("hostname", "domain-resource-default.example.com\n"))

			// Changing the domain must update the existing config map.
			resource.Spec.Domain = "example.org"
			Expect(reconciler.reconcileHostnameConfigMap(ctx, resource)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      manifests.HostnameConfigMapName(resource),
				Namespace: "default",
			}, cm)).To(Succeed())
			Expect(cm.Data).To(HaveKeyWithValue("hostname", "domain-resource-default.example.org\n"))
		})
	})

	Context("When reconciling the hostname DNS record", func() {
		It("should target the LoadBalancer endpoint with a CNAME or A record", func() {
			script := hostRecordScript("ipa-freeipa-test", "example.test", "lb.example.com", "", "pa-ss")
			for _, want := range []string{
				"ipa dnsrecord-show \"example.test\" \"ipa-freeipa-test\"",
				"ipa dnsrecord-del \"example.test\" \"ipa-freeipa-test\" --cname-rec=\"$existing_cname\"",
				"ipa dnsrecord-add \"example.test\" \"ipa-freeipa-test\" --cname-rec=\"lb.example.com.\"",
				"kinit -R admin",
				"nochange",
				"updated",
			} {
				Expect(script).To(ContainSubstring(want))
			}

			script = hostRecordScript("ipa-freeipa-test", "example.test", "", "192.0.2.10", "pa-ss")
			Expect(script).To(ContainSubstring(`ipa dnsrecord-add "example.test" "ipa-freeipa-test" --a-rec="192.0.2.10"`))
			Expect(script).NotTo(ContainSubstring(`ipa dnsrecord-add "example.test" "ipa-freeipa-test" --cname-rec=`))
			Expect(script).To(ContainSubstring(`ipa dnsrecord-del "example.test" "ipa-freeipa-test" --cname-rec="$existing_cname"`))
		})
	})

	Context("When reconciling the UDP SRV records", func() {
		It("should point the UDP SRV records at the UDP LoadBalancer endpoint", func() {
			script := udpEndpointScript("example.test", "ipa-freeipa-test-udp", "udp-lb.example.com", "", "ipa-freeipa-test-udp.example.test", "pa-ss")
			for _, want := range []string{
				"kinit -R admin",
				`ipa dnsrecord-add "example.test" "ipa-freeipa-test-udp" --cname-rec="udp-lb.example.com."`,
				"_kerberos._udp _kerberos-master._udp _kpasswd._udp",
				`ipa dnsrecord-del "example.test" "$rec" --srv-rec="$prio $weight $port $target"`,
				`ipa dnsrecord-add "example.test" "$rec" --srv-rec="$prio $weight $port ipa-freeipa-test-udp.example.test."`,
				"nochange",
				"updated",
			} {
				Expect(script).To(ContainSubstring(want))
			}

			script = udpEndpointScript("example.test", "ipa-freeipa-test-udp", "", "192.0.2.11", "ipa-freeipa-test-udp.example.test", "pa-ss")
			Expect(script).To(ContainSubstring(`ipa dnsrecord-add "example.test" "ipa-freeipa-test-udp" --a-rec="192.0.2.11"`))
			Expect(script).NotTo(ContainSubstring(`--cname-rec="udp-lb.example.com."`))
		})

		It("should remove the UDP record and restore SRV targets without a UDP service", func() {
			script := udpEndpointScript("example.test", "ipa-freeipa-test-udp", "", "", "ipa-freeipa-test.example.test", "pa-ss")
			Expect(script).To(ContainSubstring(`ipa dnsrecord-del "example.test" "ipa-freeipa-test-udp" --del-all`))
			Expect(script).To(ContainSubstring(`$target" != "ipa-freeipa-test.example.test."`))
			Expect(script).NotTo(ContainSubstring("--cname-rec="))
		})
	})
})
