// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeStripeEnsurer records calls to EnsureCustomer so tests can assert
// what the BillingAccountReconciler pushed to Stripe without standing
// up an HTTP mock server.
type fakeStripeEnsurer struct {
	mu    sync.Mutex
	calls []ensureCustomerCall
	// stubID is returned from EnsureCustomer unless overridden. Defaults
	// to whatever existingID the caller passed (since we're updating).
	stubID string
	// stubErr, when non-nil, is returned alongside the resolved ID. Used
	// to simulate Stripe API failures (e.g. tax_id rejection wrapped in
	// a *stripeinternal.TaxIDError).
	stubErr error
}

type ensureCustomerCall struct {
	existingID         string
	billingAccountName string
	details            stripeinternal.CustomerDetails
}

func (f *fakeStripeEnsurer) EnsureCustomer(_ context.Context, existingID, billingAccountName string, details stripeinternal.CustomerDetails) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ensureCustomerCall{
		existingID:         existingID,
		billingAccountName: billingAccountName,
		details:            details,
	})
	id := existingID
	if f.stubID != "" {
		id = f.stubID
	}
	return id, f.stubErr
}

func (f *fakeStripeEnsurer) Calls() []ensureCustomerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]ensureCustomerCall, len(f.calls))
	copy(cp, f.calls)
	return cp
}

var _ = Describe("BillingAccountReconciler", func() {
	It("is a no-op when no StripePaymentMethod has recorded a customer ID for the BillingAccount", func() {
		ns := "default"
		baName := "ba-noop"

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo: &billingv1alpha1.BillingContactInfo{
					Email: "noop@example.com",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ba)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ba) })

		fake := &fakeStripeEnsurer{}
		r := &BillingAccountReconciler{
			Client: k8sClient,
			stripeClientFactory: func(_ *stripeinternal.ResolvedConfig) stripeCustomerEnsurer {
				return fake
			},
		}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Calls()).To(BeEmpty(),
			"reconcile must not call Stripe when no Customer has been created yet")
	})

	It("pushes the BillingAccount spec to Stripe when a sibling StripePaymentMethod has a customer ID", func() {
		ns := "default"
		baName := "ba-sync"

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo: &billingv1alpha1.BillingContactInfo{
					Email:        "sync@example.com",
					Name:         "Test User",
					BusinessName: "Test Co.",
					InvoiceEmails: []string{
						"ar@example.com",
						"finance@example.com",
					},
				},
				TaxIDs: []billingv1alpha1.TaxID{
					{Type: "gb_vat", Value: "GB123456789"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ba)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ba) })

		// Existing PaymentMethod + StripePaymentMethod for this BA,
		// with the customer ID already stamped on status. This is the
		// state after a successful first-card add.
		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-sync-first", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "Existing Card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{
					Name: "stripe-default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pm) })

		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-sync-first", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		Expect(k8sClient.Create(ctx, spm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, spm) })

		spm.Status.StripeCustomerID = "cus_sync_001"
		Expect(k8sClient.Status().Update(ctx, spm)).To(Succeed())

		// Substituting the factory short-circuits ResolveConfig so the
		// test doesn't need a StripeProviderConfig or STRIPE_SECRET_KEY
		// env var in envtest.
		fake := &fakeStripeEnsurer{}
		r := &BillingAccountReconciler{
			Client: k8sClient,
			stripeClientFactory: func(_ *stripeinternal.ResolvedConfig) stripeCustomerEnsurer {
				return fake
			},
		}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Calls()).To(HaveLen(1))

		call := fake.Calls()[0]
		Expect(call.existingID).To(Equal("cus_sync_001"),
			"must call EnsureCustomer with the existing customer ID, not empty")
		Expect(call.billingAccountName).To(Equal(baName))
		Expect(call.details.Name).To(Equal("Test Co."),
			"BusinessName should win over Name when both are set, per customerDetailsFromBillingAccount")
		Expect(call.details.Email).To(Equal("ar@example.com"),
			"first InvoiceEmail should win over ContactInfo.Email")
		Expect(call.details.TaxIDs).To(HaveLen(1))
		Expect(call.details.TaxIDs[0].Type).To(Equal("gb_vat"))
		Expect(call.details.TaxIDs[0].Value).To(Equal("GB123456789"))
	})

	It("propagates the BA's default PaymentMethod onto CustomerDetails", func() {
		// Regression: the BA reconciler used to ignore
		// spec.defaultPaymentMethodRef entirely, so flipping the
		// consumer-side default never reached Stripe and
		// auto-invoices kept charging whichever card Stripe
		// happened to have on file.
		ns := "default"
		baName := "ba-default-sync"

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo: &billingv1alpha1.BillingContactInfo{
					Email: "default-sync@example.com",
				},
				DefaultPaymentMethodRef: &billingv1alpha1.DefaultPaymentMethodRef{
					Name: "pm-default-sync",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ba)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ba) })

		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-default-sync", Namespace: ns},
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
			ObjectMeta: metav1.ObjectMeta{Name: "pm-default-sync", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		Expect(k8sClient.Create(ctx, spm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, spm) })

		spm.Status.StripeCustomerID = "cus_default_sync"
		spm.Status.StripePaymentMethodID = "pm_1AbCdEfGhIjKlMnO"
		Expect(k8sClient.Status().Update(ctx, spm)).To(Succeed())

		fake := &fakeStripeEnsurer{}
		r := &BillingAccountReconciler{
			Client: k8sClient,
			stripeClientFactory: func(_ *stripeinternal.ResolvedConfig) stripeCustomerEnsurer {
				return fake
			},
		}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Calls()).To(HaveLen(1))
		Expect(fake.Calls()[0].details.DefaultPaymentMethodID).To(Equal("pm_1AbCdEfGhIjKlMnO"),
			"BA's defaultPaymentMethodRef must resolve to the SPM's stripe pm_… ID and flow into EnsureCustomer")
	})

	It("propagates an empty default when the BA has no defaultPaymentMethodRef", func() {
		// Symmetric of the case above: when the consumer un-sets the
		// default we must send an empty string to Stripe so its
		// invoice_settings.default_payment_method clears, rather than
		// silently leaving stale data upstream.
		ns := "default"
		baName := "ba-default-cleared"

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo:  &billingv1alpha1.BillingContactInfo{Email: "cleared@example.com"},
				// DefaultPaymentMethodRef intentionally nil.
			},
		}
		Expect(k8sClient.Create(ctx, ba)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ba) })

		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-cleared-default", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "Some card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{Name: "stripe-default"},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pm) })

		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-cleared-default", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		Expect(k8sClient.Create(ctx, spm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, spm) })

		spm.Status.StripeCustomerID = "cus_cleared"
		spm.Status.StripePaymentMethodID = "pm_2XyZ"
		Expect(k8sClient.Status().Update(ctx, spm)).To(Succeed())

		fake := &fakeStripeEnsurer{}
		r := &BillingAccountReconciler{
			Client: k8sClient,
			stripeClientFactory: func(_ *stripeinternal.ResolvedConfig) stripeCustomerEnsurer {
				return fake
			},
		}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Calls()).To(HaveLen(1))
		Expect(fake.Calls()[0].details.DefaultPaymentMethodID).To(BeEmpty(),
			"un-set default on BA must propagate as empty string to clear Stripe's default")
	})

	It("treats a TaxIDError from EnsureCustomer as non-fatal", func() {
		ns := "default"
		baName := "ba-taxerr"

		ba := &billingv1alpha1.BillingAccount{
			ObjectMeta: metav1.ObjectMeta{Name: baName, Namespace: ns},
			Spec: billingv1alpha1.BillingAccountSpec{
				CurrencyCode: "USD",
				ContactInfo: &billingv1alpha1.BillingContactInfo{
					Email: "taxerr@example.com",
				},
				TaxIDs: []billingv1alpha1.TaxID{
					{Type: "gb_vat", Value: "GB12323234"}, // bad — 8 digits
				},
			},
		}
		Expect(k8sClient.Create(ctx, ba)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ba) })

		pm := &billingv1alpha1.PaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-taxerr", Namespace: ns},
			Spec: billingv1alpha1.PaymentMethodSpec{
				BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: baName},
				DisplayName:       "Card",
				PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{
					Name: "stripe-default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, pm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pm) })

		spm := &stripev1alpha1.StripePaymentMethod{
			ObjectMeta: metav1.ObjectMeta{Name: "pm-taxerr", Namespace: ns},
			Spec: stripev1alpha1.StripePaymentMethodSpec{
				PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
			},
		}
		Expect(k8sClient.Create(ctx, spm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, spm) })

		spm.Status.StripeCustomerID = "cus_taxerr_001"
		Expect(k8sClient.Status().Update(ctx, spm)).To(Succeed())

		// EnsureCustomer returns the customer ID alongside a TaxIDError —
		// the exact shape stripe-provider/internal/stripe returns when
		// Stripe rejects a tax_id on a successfully-created Customer.
		fake := &fakeStripeEnsurer{
			stubErr: &stripeinternal.TaxIDError{
				Underlying: fmt.Errorf("creating tax_id gb_vat=GB12323234: invalid value"),
			},
		}
		r := &BillingAccountReconciler{
			Client: k8sClient,
			stripeClientFactory: func(_ *stripeinternal.ResolvedConfig) stripeCustomerEnsurer {
				return fake
			},
		}

		// The reconciler must not surface a TaxIDError as a reconcile
		// failure. If it did, controller-runtime would retry forever
		// while the user can't correct their tax ID (and the rest of
		// the customer record is already in sync).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred(),
			"tax-ID rejection from Stripe must not fail the BA reconcile")
		Expect(fake.Calls()).To(HaveLen(1),
			"reconcile must still attempt EnsureCustomer once")
	})
})

