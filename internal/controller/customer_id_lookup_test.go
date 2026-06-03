// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("findExistingCustomerID", func() {
	It("returns the customer ID stamped on a sibling StripePaymentMethod for the same BillingAccount", func() {
		ns := "default"
		baName := "ba-find-1"

		// First PaymentMethod for this BA, already reconciled past
		// AwaitingConfirmation so the child carries a Stripe customer
		// ID on status.
		firstPM := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-find-first", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "First Card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{
					Name: "stripe-default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, firstPM)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, firstPM) })

		firstSPM := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-find-first", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: firstPM.Name},
			},
		}
		Expect(k8sClient.Create(ctx, firstSPM)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, firstSPM) })

		// Status updates have to go through the status subresource.
		firstSPM.Status.StripeCustomerID = "cus_existing_001"
		Expect(k8sClient.Status().Update(ctx, firstSPM)).To(Succeed())

		got, err := findExistingCustomerID(ctx, k8sClient, ns, baName)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("cus_existing_001"))
	})

	It("returns empty when no sibling carries a customer ID", func() {
		ns := "default"
		baName := "ba-find-empty"

		// PaymentMethod exists but the StripePaymentMethod child hasn't
		// reached the customer-creation step yet, so status is empty.
		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-find-empty", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "Empty Card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{
					Name: "stripe-default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pm) })

		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-find-empty", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		Expect(k8sClient.Create(ctx, spm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, spm) })

		got, err := findExistingCustomerID(ctx, k8sClient, ns, baName)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("ignores siblings whose parent PaymentMethod targets a different BillingAccount", func() {
		ns := "default"
		ourBA := "ba-find-ours"
		otherBA := "ba-find-other"

		// A StripePaymentMethod that belongs to a *different* BA but
		// happens to live in the same namespace. Its customer ID must
		// not be returned to a query for ours.
		otherPM := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-find-other", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: otherBA},
				DisplayName:       "Other Card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{
					Name: "stripe-default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, otherPM)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, otherPM) })

		otherSPM := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-find-other", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: otherPM.Name},
			},
		}
		Expect(k8sClient.Create(ctx, otherSPM)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, otherSPM) })

		otherSPM.Status.StripeCustomerID = "cus_other_999"
		Expect(k8sClient.Status().Update(ctx, otherSPM)).To(Succeed())

		got, err := findExistingCustomerID(ctx, k8sClient, ns, ourBA)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("skips siblings whose parent PaymentMethod has been deleted", func() {
		ns := "default"
		baName := "ba-find-orphan"

		// Orphan StripePaymentMethod: there's no PaymentMethod in
		// cluster for the spec.paymentMethodRef.name. The lookup must
		// skip it rather than failing.
		orphanSPM := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-find-orphan", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: "pm-missing"},
			},
		}
		Expect(k8sClient.Create(ctx, orphanSPM)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, orphanSPM) })

		orphanSPM.Status.StripeCustomerID = "cus_orphan_111"
		Expect(k8sClient.Status().Update(ctx, orphanSPM)).To(Succeed())

		got, err := findExistingCustomerID(ctx, k8sClient, ns, baName)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("scopes the lookup to the requested namespace", func() {
		// Stamp the same BA name in two namespaces to confirm the
		// list query stays scoped. The "other" namespace's customer ID
		// must not leak.
		baName := "ba-find-ns-scope"
		otherNs := "ns-other-scope"

		ensureNamespace(otherNs)

		otherPM := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-other-ns", Namespace: otherNs},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "Foreign Card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{
					Name: "stripe-default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, otherPM)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, otherPM) })

		otherSPM := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-other-ns", Namespace: otherNs},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: otherPM.Name},
			},
		}
		Expect(k8sClient.Create(ctx, otherSPM)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, otherSPM) })

		otherSPM.Status.StripeCustomerID = "cus_other_ns_555"
		Expect(k8sClient.Status().Update(ctx, otherSPM)).To(Succeed())

		got, err := findExistingCustomerID(ctx, k8sClient, "default", baName)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())

		// Sanity: the same lookup against the other namespace returns
		// the stamped ID, confirming the test fixture wired the data
		// correctly.
		got, err = findExistingCustomerID(ctx, k8sClient, otherNs, baName)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("cus_other_ns_555"))
	})
})

var _ = Describe("findDefaultStripePaymentMethodID", func() {
	It("returns the Stripe pm_… ID for the BA's default PaymentMethod once the SPM is confirmed", func() {
		ns := "default"
		baName := "ba-default-resolved"

		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-default-resolved", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "Default card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{
					Name: "stripe-default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pm) })

		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-default-resolved", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		Expect(k8sClient.Create(ctx, spm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, spm) })

		spm.Status.StripePaymentMethodID = "pm_1AbCdEfGhIjKlMnO"
		Expect(k8sClient.Status().Update(ctx, spm)).To(Succeed())

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo:  &billingv1alpha1.BillingContactInfo{Email: "default@example.com"},
				DefaultPaymentMethodRef: &billingv1alpha1.DefaultPaymentMethodRef{
					Name: pm.Name,
				},
			},
		}
		got, err := findDefaultStripePaymentMethodID(ctx, k8sClient, ba)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("pm_1AbCdEfGhIjKlMnO"))
	})

	It("returns empty when no default is set on the BillingAccount", func() {
		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "ba-no-default", Namespace: "default"},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo:  &billingv1alpha1.BillingContactInfo{Email: "n@example.com"},
				// DefaultPaymentMethodRef nil
			},
		}
		got, err := findDefaultStripePaymentMethodID(ctx, k8sClient, ba)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("returns empty when the SPM hasn't been confirmed yet (no stripePaymentMethodId on status)", func() {
		// Consumer-side validation rejects setting an unconfirmed PM as
		// default, but a brand-new card whose webhook hasn't landed yet
		// can momentarily be in this state. The reconciler relies on
		// "" here to clear Stripe's default until the SPM catches up.
		ns := "default"
		baName := "ba-default-pending"

		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-default-pending", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "Pending card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{Name: "stripe-default"},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pm) })

		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-default-pending", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		Expect(k8sClient.Create(ctx, spm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, spm) })
		// Deliberately leave status.stripePaymentMethodId unset.

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo:  &billingv1alpha1.BillingContactInfo{Email: "p@example.com"},
				DefaultPaymentMethodRef: &billingv1alpha1.DefaultPaymentMethodRef{Name: pm.Name},
			},
		}
		got, err := findDefaultStripePaymentMethodID(ctx, k8sClient, ba)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("returns empty when the SPM referenced by the BA doesn't exist", func() {
		// The BA references a PM that has no SPM yet (or the SPM was
		// deleted out from under us). Either way the resolver must
		// return "" so the controller clears Stripe's default rather
		// than erroring forever.
		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "ba-dangling-default", Namespace: "default"},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode:            "USD",
				ContactInfo:             &billingv1alpha1.BillingContactInfo{Email: "d@example.com"},
				DefaultPaymentMethodRef: &billingv1alpha1.DefaultPaymentMethodRef{Name: "pm-does-not-exist"},
			},
		}
		got, err := findDefaultStripePaymentMethodID(ctx, k8sClient, ba)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})

var _ = Describe("listBoundProjectNames", func() {
	It("returns the projects of every Active binding pointing at the BA, sorted lexicographically", func() {
		ns := "default"
		baName := "ba-bound-projects"

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo:  &billingv1alpha1.BillingContactInfo{Email: "bound@example.com"},
			},
		}
		Expect(k8sClient.Create(ctx, ba)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ba) })

		// Three matching bindings (one Active, one Pending, one
		// Active pointing at a different BA) plus the sort assertion.
		mk := func(name, baRef, projectRef string, phase billingv1alpha1.BillingAccountBindingPhase) *billingv1alpha1.BillingAccountBinding {
			b := &billingv1alpha1.BillingAccountBinding{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: billingv1alpha1.BillingAccountBindingSpec{
					BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baRef},
					ProjectRef:        billingv1alpha1.ProjectRef{Name: projectRef},
				},
			}
			Expect(k8sClient.Create(ctx, b)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, b) })
			b.Status.Phase = phase
			Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
			return b
		}
		mk("bab-zeta", baName, "zeta-project", billingv1alpha1.BillingAccountBindingPhaseActive)
		mk("bab-alpha", baName, "alpha-project", billingv1alpha1.BillingAccountBindingPhaseActive)
		mk("bab-superseded", baName, "superseded-project", billingv1alpha1.BillingAccountBindingPhaseSuperseded)
		mk("bab-other-ba", "ba-someone-else", "ignored-project", billingv1alpha1.BillingAccountBindingPhaseActive)

		got, err := listBoundProjectNames(ctx, k8sClient, ba)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]string{"alpha-project", "zeta-project"}),
			"only Active bindings pointing at this BA should appear, in sorted order")
	})

	It("returns empty when the BA has no bindings", func() {
		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "ba-unbound", Namespace: "default"},
		}
		got, err := listBoundProjectNames(ctx, k8sClient, ba)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})

// ensureNamespace makes a Namespace if it doesn't already exist. Used
// by tests that need to verify the lookup is namespace-scoped — the
// fixtures need a second namespace to live in.
func ensureNamespace(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}
