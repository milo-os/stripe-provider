// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// statefulStripe is a fake Stripe that remembers the customer it
// created, like the real one: a call without an existing ID creates
// "cus_new", and is slow enough that concurrent callers overlap.
type statefulStripe struct {
	mu      sync.Mutex
	creates int
	err     error
}

func (f *statefulStripe) EnsureCustomer(_ context.Context, existingID string, _ stripeinternal.BillingAccountKey, _ stripeinternal.CustomerDetails) (string, error) {
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if existingID == "" {
		f.creates++
		return "cus_new", nil
	}
	return existingID, nil
}

func (f *statefulStripe) Creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

// invoiceBilledAccount creates a BillingAccount that billing reports as
// billed by invoice, with no payment method.
func invoiceBilledAccount(name string) *billingv1alpha1.BillingAccount {
	return invoiceBilledAccountIn("default", name)
}

func invoiceBilledAccountIn(namespace, name string) *billingv1alpha1.BillingAccount {
	ba := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: billingv1alpha1.BillingAccountSpec{
			CurrencyCode: "USD",
			ContactInfo:  &billingv1alpha1.BillingContactInfo{Email: name + "@example.com"},
		},
	}
	Expect(k8sClient.Create(ctx, ba)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, ba) })
	ba.Status.PaymentArrangement = &billingv1alpha1.BillingAccountPaymentArrangement{
		Name: name + "-terms",
		Type: billingv1alpha1.BillingArrangementTypeInvoice,
		Invoice: &billingv1alpha1.InvoiceArrangementTerms{
			NetDays:     30,
			CreditLimit: "10000",
		},
	}
	Expect(k8sClient.Status().Update(ctx, ba)).To(Succeed())
	return ba
}

func reconcilerWith(stripe stripeCustomerEnsurer) *BillingAccountReconciler {
	return &BillingAccountReconciler{
		Client: k8sClient,
		stripeClientFactory: func(_ *stripeinternal.ResolvedConfig) stripeCustomerEnsurer {
			return stripe
		},
	}
}

func getStripeCustomer(ba *billingv1alpha1.BillingAccount) *stripev1alpha1.StripeCustomer {
	var sc stripev1alpha1.StripeCustomer
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ba), &sc)).To(Succeed())
	return &sc
}

var _ = Describe("StripeCustomer", func() {
	It("creates a Stripe Customer for an account billed by invoice that has no card", func() {
		ba := invoiceBilledAccount("ba-invoice-only")
		fake := &fakeStripeEnsurer{stubID: "cus_invoice"}

		_, err := reconcilerWith(fake).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred())

		Expect(fake.Calls()).To(HaveLen(1))
		Expect(fake.Calls()[0].existingID).To(BeEmpty())
		Expect(fake.Calls()[0].account).To(Equal(stripeinternal.BillingAccountKey{
			Namespace: "default", Name: ba.Name, UID: string(ba.UID),
		}))

		sc := getStripeCustomer(ba)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sc) })
		Expect(sc.Spec.BillingAccountRef.Name).To(Equal(ba.Name))
		Expect(metav1.GetControllerOf(sc).UID).To(Equal(ba.UID),
			"the StripeCustomer is owned by its account, so it's garbage collected with it")
		Expect(sc.Status.StripeCustomerID).To(Equal("cus_invoice"))
		Expect(apimeta.IsStatusConditionTrue(sc.Status.Conditions, stripev1alpha1.StripeCustomerConditionReady)).To(BeTrue())
		Expect(apimeta.IsStatusConditionTrue(sc.Status.Conditions, stripev1alpha1.StripeCustomerConditionTaxIDsApplied)).To(BeTrue())
	})

	It("keeps using the published customer once the account stops paying by invoice", func() {
		ba := invoiceBilledAccount("ba-terms-ended")
		fake := &fakeStripeEnsurer{stubID: "cus_terms"}
		r := reconcilerWith(fake)
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)}
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, getStripeCustomer(ba)) })

		ba.Status.PaymentArrangement = nil
		Expect(k8sClient.Status().Update(ctx, ba)).To(Succeed())
		fake.stubID = ""

		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Calls()).To(HaveLen(2), "an account with a StripeCustomer keeps syncing its customer")
		Expect(fake.Calls()[1].existingID).To(Equal("cus_terms"))
	})

	It("prints the payment terms' PO number and agreement on invoices while they're in effect", func() {
		ba := invoiceBilledAccount("ba-invoice-fields")
		ba.Status.PaymentArrangement.Invoice.PurchaseOrderNumber = "PO-4471"
		ba.Status.PaymentArrangement.Invoice.AgreementReference = "MSA-2026-014"
		Expect(k8sClient.Status().Update(ctx, ba)).To(Succeed())
		fake := &fakeStripeEnsurer{stubID: "cus_fields"}
		r := reconcilerWith(fake)
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)}

		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, getStripeCustomer(ba)) })
		Expect(fake.Calls()[0].details.InvoiceFields).To(Equal([]stripeinternal.InvoiceField{
			{Name: "PO number", Value: "PO-4471"},
			{Name: "Agreement", Value: "MSA-2026-014"},
		}))

		ba.Status.PaymentArrangement = nil
		Expect(k8sClient.Status().Update(ctx, ba)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Calls()[1].details.InvoiceFields).To(BeEmpty(), "the fields are cleared once the terms end")
	})

	It("resyncs the customer when the references on the terms change", func() {
		before := &billingv1alpha1.BillingAccount{Status: billingv1alpha1.BillingAccountStatus{
			PaymentArrangement: &billingv1alpha1.BillingAccountPaymentArrangement{
				Name: "terms", Type: billingv1alpha1.BillingArrangementTypeInvoice,
				Invoice: &billingv1alpha1.InvoiceArrangementTerms{NetDays: 30, PurchaseOrderNumber: "PO-1"},
			},
		}}
		poChanged := before.DeepCopy()
		poChanged.Status.PaymentArrangement.Invoice.PurchaseOrderNumber = "PO-2"
		netDaysChanged := before.DeepCopy()
		netDaysChanged.Status.PaymentArrangement.Invoice.NetDays = 45

		Expect(invoiceArrangementChanged.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: poChanged})).To(BeTrue())
		Expect(invoiceArrangementChanged.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: netDaysChanged})).To(BeFalse(),
			"net days don't reach the Stripe customer")
	})

	It("records a tax ID Stripe rejected without failing the customer", func() {
		ba := invoiceBilledAccount("ba-invoice-taxerr")
		fake := &fakeStripeEnsurer{
			stubID:  "cus_taxerr",
			stubErr: &stripeinternal.TaxIDError{Underlying: fmt.Errorf("invalid gb_vat")},
		}

		_, err := reconcilerWith(fake).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred())

		sc := getStripeCustomer(ba)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sc) })
		Expect(sc.Status.StripeCustomerID).To(Equal("cus_taxerr"))
		Expect(apimeta.IsStatusConditionTrue(sc.Status.Conditions, stripev1alpha1.StripeCustomerConditionReady)).To(BeTrue())
		taxIDs := apimeta.FindStatusCondition(sc.Status.Conditions, stripev1alpha1.StripeCustomerConditionTaxIDsApplied)
		Expect(taxIDs).NotTo(BeNil())
		Expect(taxIDs.Status).To(Equal(metav1.ConditionFalse))
		Expect(taxIDs.Reason).To(Equal("StripeRejected"))
	})

	It("reports a Stripe failure on the StripeCustomer and retries", func() {
		ba := invoiceBilledAccount("ba-invoice-stripe-down")
		fake := &statefulStripe{err: errors.New("stripe is down")}

		_, err := reconcilerWith(fake).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).To(MatchError(ContainSubstring("stripe is down")))

		sc := getStripeCustomer(ba)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sc) })
		Expect(sc.Status.StripeCustomerID).To(BeEmpty())
		ready := apimeta.FindStatusCondition(sc.Status.Conditions, stripev1alpha1.StripeCustomerConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("StripeError"))
	})

	It("won't bill an account to a StripeCustomer left by an earlier account with the same name", func() {
		ba := invoiceBilledAccount("ba-recreated")
		stale := &stripev1alpha1.StripeCustomer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ba.Name,
				Namespace: ba.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: billingv1alpha1.GroupVersion.String(),
					Kind:       "BillingAccount",
					Name:       ba.Name,
					UID:        types.UID("uid-of-the-deleted-account"),
					Controller: new(true),
				}},
			},
			Spec: stripev1alpha1.StripeCustomerSpec{
				BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: ba.Name},
			},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, stale) })
		fake := &fakeStripeEnsurer{}

		_, err := reconcilerWith(fake).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).To(MatchError(ContainSubstring("waiting for it to be garbage collected")))
		Expect(fake.Calls()).To(BeEmpty())
	})

	It("skips an account whose namespace is being deleted instead of retrying", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "organization-terminating"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		ba := invoiceBilledAccountIn(ns.Name, "ba-terminating")
		// envtest runs no namespace controller, so the namespace stays
		// Terminating and the API server refuses new objects in it.
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		fake := &fakeStripeEnsurer{stubID: "cus_terminating"}

		_, err := reconcilerWith(fake).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Calls()).To(BeEmpty())
	})

	It("creates one Stripe Customer when a card and payment terms arrive together", func() {
		ba := invoiceBilledAccount("ba-race")
		fake := &statefulStripe{}
		customers := NewStripeCustomers(k8sClient, k8sClient)

		var wg sync.WaitGroup
		ids := make([]string, 4)
		for i := range ids {
			wg.Go(func() {
				defer GinkgoRecover()
				id, err := customers.Ensure(ctx, fake, ba)
				Expect(err).NotTo(HaveOccurred())
				ids[i] = id
			})
		}
		wg.Wait()
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, getStripeCustomer(ba)) })

		Expect(fake.Creates()).To(Equal(1))
		Expect(ids).To(HaveEach("cus_new"))
	})
})
