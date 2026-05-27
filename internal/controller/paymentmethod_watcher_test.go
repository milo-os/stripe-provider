// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PaymentMethodWatcher", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	It("creates a StripePaymentMethod child for a PaymentMethod whose class names stripe", func() {
		// PaymentMethodClass with provider=stripe.
		class := &billingv1alpha1.PaymentMethodClass{
			ObjectMeta: metav1.ObjectMeta{Name: "stripe-watcher"},
			Spec: billingv1alpha1.PaymentMethodClassSpec{
				Provider: "stripe",
				ParametersRef: billingv1alpha1.PaymentMethodClassParametersRef{
					Group: "stripe.billing.miloapis.com",
					Kind:  "StripeProviderConfig",
					Name:  "default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, class)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, class)).To(Succeed())
		})

		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-stripe-1", Namespace: "default"},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef:     billingv1alpha1.BillingAccountRef{Name: "ba-1"},
				DisplayName:           "Card 1",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{Name: class.Name},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, pm)
		})

		Eventually(func(g Gomega) {
			var child stripev1alpha1.StripePaymentMethod
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: pm.Namespace, Name: pm.Name}, &child)).To(Succeed())
			g.Expect(child.Spec.PaymentMethodRef.Name).To(Equal(pm.Name))
			g.Expect(child.OwnerReferences).To(HaveLen(1))
			g.Expect(child.OwnerReferences[0].Kind).To(Equal("PaymentMethod"))
			g.Expect(child.OwnerReferences[0].Name).To(Equal(pm.Name))
			g.Expect(*child.OwnerReferences[0].Controller).To(BeTrue())
		}, timeout, interval).Should(Succeed())
	})

	It("does not create a child when the class names a different provider", func() {
		class := &billingv1alpha1.PaymentMethodClass{
			ObjectMeta: metav1.ObjectMeta{Name: "braintree-watcher"},
			Spec: billingv1alpha1.PaymentMethodClassSpec{
				Provider: "braintree",
				ParametersRef: billingv1alpha1.PaymentMethodClassParametersRef{
					Group: "braintree.billing.miloapis.com",
					Kind:  "BraintreeProviderConfig",
					Name:  "default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, class)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, class)).To(Succeed())
		})

		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-braintree", Namespace: "default"},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef:     billingv1alpha1.BillingAccountRef{Name: "ba-1"},
				DisplayName:           "Braintree card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{Name: class.Name},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, pm)
		})

		// Wait one reconcile interval, then assert no child was created.
		Consistently(func(g Gomega) {
			var child stripev1alpha1.StripePaymentMethod
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: pm.Namespace, Name: pm.Name}, &child)
			g.Expect(err).To(HaveOccurred(), "expected no StripePaymentMethod to exist")
		}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
	})
})
