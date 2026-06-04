// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Mapping-function tests for the BillingAccountReconciler's secondary
// watches. The Watches() wiring itself is a one-liner builder call; the
// load-bearing logic is the map from a watched object back to a
// BillingAccount reconcile request, and that's what these tests cover.

var _ = Describe("BillingAccountReconciler.enqueueBillingAccountForBinding", func() {
	r := &BillingAccountReconciler{Client: nil}

	It("maps an Active binding to its referenced BillingAccount in the same namespace", func() {
		bab := &billingv1alpha1.BillingAccountBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "bab-1", Namespace: "organization-acme"},
			Spec: billingv1alpha1.BillingAccountBindingSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "ba-main"},
				ProjectRef:        billingv1alpha1.ProjectRef{Name: "p-1"},
			},
		}
		got := r.enqueueBillingAccountForBinding(ctx, bab)
		Expect(got).To(HaveLen(1))
		Expect(got[0].Namespace).To(Equal("organization-acme"))
		Expect(got[0].Name).To(Equal("ba-main"))
	})

	It("returns no requests when the binding has no billingAccountRef", func() {
		bab := &billingv1alpha1.BillingAccountBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "bab-empty", Namespace: "organization-acme"},
		}
		Expect(r.enqueueBillingAccountForBinding(ctx, bab)).To(BeEmpty())
	})

	It("returns no requests when handed a non-BillingAccountBinding object", func() {
		Expect(r.enqueueBillingAccountForBinding(ctx, &billingv1alpha1.BillingAccount{})).To(BeEmpty())
	})
})

var _ = Describe("BillingAccountReconciler.enqueueBillingAccountForStripePaymentMethod", func() {
	It("maps an SPM to the BA referenced by its parent PaymentMethod", func() {
		ns := "default"
		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-watch", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "ba-target"},
				DisplayName:       "Card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{Name: "stripe-default"},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pm) })

		r := &BillingAccountReconciler{Client: k8sClient}
		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-watch", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		got := r.enqueueBillingAccountForStripePaymentMethod(ctx, spm)
		Expect(got).To(HaveLen(1))
		Expect(got[0].Namespace).To(Equal(ns))
		Expect(got[0].Name).To(Equal("ba-target"))
	})

	It("returns no requests when the SPM's parent PaymentMethod has been deleted", func() {
		// Orphan SPM (no parent PM in cluster). The mapper has to look
		// up the parent to find the BA ref; a missing parent should
		// drop the event rather than enqueue a request the reconciler
		// can't satisfy.
		r := &BillingAccountReconciler{Client: k8sClient}
		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "spm-orphan", Namespace: "default"},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: "pm-does-not-exist"},
			},
		}
		Expect(r.enqueueBillingAccountForStripePaymentMethod(ctx, spm)).To(BeEmpty())
	})

	It("returns no requests when the SPM has no paymentMethodRef", func() {
		r := &BillingAccountReconciler{Client: k8sClient}
		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "spm-no-ref", Namespace: "default"},
		}
		Expect(r.enqueueBillingAccountForStripePaymentMethod(ctx, spm)).To(BeEmpty())
	})

	It("returns no requests when handed a non-StripePaymentMethod object", func() {
		r := &BillingAccountReconciler{Client: k8sClient}
		Expect(r.enqueueBillingAccountForStripePaymentMethod(ctx, &billingv1alpha1.PaymentMethod{})).To(BeEmpty())
	})
})

var _ client.Object = &billingv1alpha1.BillingAccountBinding{}
var _ client.Object = &stripev1alpha1.StripePaymentMethod{}
