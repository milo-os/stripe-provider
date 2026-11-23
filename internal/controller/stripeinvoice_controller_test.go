// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeInvoicer stands in for Stripe's invoice calls.
type fakeInvoicer struct {
	mu        sync.Mutex
	previews  []stripeinternal.InvoiceDraft
	drafts    []stripeinternal.InvoiceDraft
	finalized []string
	deleted   []string
	result    stripeinternal.InvoiceResult
	// stored is the invoice as Stripe has it, for RetrieveInvoice; nil is
	// one Stripe doesn't have.
	stored *stripeinternal.InvoiceResult
	err    error
	delErr error
}

func (f *fakeInvoicer) FinalizeInvoice(_ context.Context, id, _ string) (*stripeinternal.InvoiceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalized = append(f.finalized, id)
	if f.err != nil {
		return nil, f.err
	}
	res := f.result
	res.ID, res.Status, res.Number = id, "open", "DATUM-0042"
	res.HostedInvoiceURL = "https://invoice.stripe.com/i/" + id
	res.FinalizedAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	res.AmountRemaining = res.AmountDue
	f.stored = &res
	return &res, nil
}

func (f *fakeInvoicer) RetrieveInvoice(_ context.Context, id string) (*stripeinternal.InvoiceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stored == nil {
		return nil, errors.Join(stripeinternal.ErrInvoiceMissing)
	}
	res := *f.stored
	return &res, nil
}

func (f *fakeInvoicer) PreviewInvoice(_ context.Context, d stripeinternal.InvoiceDraft) (*stripeinternal.InvoiceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previews = append(f.previews, d)
	if f.err != nil {
		return nil, f.err
	}
	res := f.result
	return &res, nil
}

func (f *fakeInvoicer) EnsureDraftInvoice(_ context.Context, _ string, d stripeinternal.InvoiceDraft) (*stripeinternal.InvoiceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drafts = append(f.drafts, d)
	if f.err != nil {
		return &stripeinternal.InvoiceResult{ID: "in_partial"}, f.err
	}
	res := f.result
	res.ID, res.Status = "in_draft", "draft"
	f.stored = &res
	return &res, nil
}

func (f *fakeInvoicer) DeleteDraftInvoice(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return f.delErr
}

func (f *fakeInvoicer) DashboardInvoiceURL(id string) string {
	return "https://dashboard.stripe.com/test/invoices/" + id
}

var _ = Describe("StripeInvoice", func() {
	const ns = "stripe-invoices"
	const pricingNS = "stripe-invoices-pricing"
	var (
		stripe *fakeInvoicer
		now    time.Time
	)

	reconciler := func() *StripeInvoiceReconciler {
		return &StripeInvoiceReconciler{
			Client:              k8sClient,
			Now:                 func() time.Time { return now },
			stripeClientFactory: func() stripeInvoicer { return stripe },
		}
	}
	reconcileInvoice := func(inv *stripev1alpha1.StripeInvoice) (reconcile.Result, error) {
		return reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(inv)})
	}
	get := func(inv *stripev1alpha1.StripeInvoice) *stripev1alpha1.StripeInvoice {
		out := &stripev1alpha1.StripeInvoice{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inv), out)).To(Succeed())
		return out
	}

	// readyCustomer and readyProduct give the account a Stripe customer and
	// the pricing a ready Stripe Product, as stripe-provider would.
	readyCustomer := func(account string) {
		sc := &stripev1alpha1.StripeCustomer{
			ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: ns},
			Spec:       stripev1alpha1.StripeCustomerSpec{BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: account}},
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		sc.Status.StripeCustomerID = "cus_" + account
		Expect(k8sClient.Status().Update(ctx, sc)).To(Succeed())
	}
	readyProduct := func(pricing string) {
		sp := &billingv1alpha1.ServicePricing{
			ObjectMeta: metav1.ObjectMeta{Name: pricing, Namespace: pricingNS},
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeRecurring, Currency: "USD", Amount: "330.00",
				Interval: billingv1alpha1.ChargeIntervalMonthly,
			},
		}
		Expect(k8sClient.Create(ctx, sp)).To(Succeed())
		product := &stripev1alpha1.StripeProduct{
			ObjectMeta: metav1.ObjectMeta{Name: stripeProductName(sp), Namespace: pricingNS},
			Spec:       stripev1alpha1.StripeProductSpec{PricingRef: &stripev1alpha1.ServicePricingLocalRef{Name: pricing}},
		}
		Expect(k8sClient.Create(ctx, product)).To(Succeed())
		product.Status.ProductID = "datum_" + pricing
		apimeta.SetStatusCondition(&product.Status.Conditions, metav1.Condition{
			Type: stripev1alpha1.StripeProductConditionReady, Status: metav1.ConditionTrue, Reason: "Synced",
		})
		Expect(k8sClient.Status().Update(ctx, product)).To(Succeed())
	}

	invoice := func(name, account, pricing string, purpose stripev1alpha1.StripeInvoicePurpose) *stripev1alpha1.StripeInvoice {
		start := metav1.NewTime(time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC))
		end := metav1.NewTime(time.Date(2027, 1, 14, 23, 59, 59, 0, time.UTC))
		return &stripev1alpha1.StripeInvoice{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: stripev1alpha1.StripeInvoiceSpec{
				BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: account},
				Purpose:           purpose,
				Lines: []stripev1alpha1.StripeInvoiceLine{{
					Key: "rack", Description: "Rack space",
					ServicePricingRef: stripev1alpha1.ServicePricingRef{Namespace: pricingNS, Name: pricing},
					Quantity:          "1.5", UnitAmount: "330.00", Amount: "495.00",
					Period:          stripev1alpha1.StripeInvoiceLinePeriod{Start: start, End: end},
					Tax:             &stripev1alpha1.StripeInvoiceLineTax{Taxable: ptr.To(false)},
					DiscountPercent: "10",
					Note:            "Install delayed a week",
				}},
			},
		}
	}

	BeforeEach(func() {
		for _, n := range []string{ns, pricingNS} {
			err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		}
		stripe = &fakeInvoicer{result: stripeinternal.InvoiceResult{
			ID: "upcoming_in_1", Subtotal: 49500, Discount: 4950, Tax: 0, Total: 44550,
			StartingBalance: 23, AmountDue: 44573,
			Lines: []stripeinternal.LineResult{{Key: "rack", Amount: 49500, Discount: 4950}},
		}}
		now = time.Now()
	})

	It("previews through Stripe and records its amounts", func() {
		readyCustomer("acme")
		readyProduct("rack-v1")
		inv := invoice("preview", "acme", "rack-v1", stripev1alpha1.StripeInvoicePurposePreview)
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())

		res, err := reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically("~", previewLifetime, time.Minute), "requeued for its expiry")

		Expect(stripe.previews).To(HaveLen(1))
		Expect(stripe.drafts).To(BeEmpty())
		sent := stripe.previews[0]
		Expect(sent.CustomerID).To(Equal("cus_acme"))
		Expect(sent.Lines).To(HaveLen(1))
		Expect(sent.Lines[0].ProductID).To(Equal("datum_rack-v1"))
		Expect(sent.Lines[0].TaxCode).To(Equal(stripeinternal.NontaxableTaxCode))
		Expect(sent.Lines[0].Quantity.String()).To(Equal("1.5"))
		Expect(sent.Lines[0].DiscountPercent.String()).To(Equal("10"))
		Expect(sent.Lines[0].Note).To(Equal("Install delayed a week"))

		got := get(inv)
		Expect(got.Status.Subtotal).To(Equal("495.00"))
		Expect(got.Status.Discount).To(Equal("49.50"))
		Expect(got.Status.Total).To(Equal("445.50"))
		Expect(got.Status.StartingBalance).To(Equal("0.23"), "an earlier invoice below Stripe's minimum")
		Expect(got.Status.AmountDue).To(Equal("445.73"))
		Expect(got.Status.InvoiceID).To(BeEmpty(), "a preview creates nothing")
		Expect(got.Status.Lines).To(ConsistOf(stripev1alpha1.StripeInvoiceLineStatus{
			Key: "rack", Amount: "495.00", Discount: "49.50", Tax: "0.00",
		}))
		Expect(got.Status.ExpiresAt).NotTo(BeNil())
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, stripev1alpha1.StripeInvoiceConditionReady)).To(BeTrue())
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, stripev1alpha1.StripeInvoiceConditionAmountsMatch)).To(BeTrue())

		// Done: a second pass doesn't ask Stripe again.
		_, err = reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(stripe.previews).To(HaveLen(1))
	})

	It("keeps the pricing's SKU in the line's metadata, not its description", func() {
		readyCustomer("cyberdyne")
		sp := &billingv1alpha1.ServicePricing{
			ObjectMeta: metav1.ObjectMeta{Name: "cabling-v1", Namespace: pricingNS},
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeRecurring, Currency: "USD", Amount: "330.00",
				Interval: billingv1alpha1.ChargeIntervalMonthly, SKU: "DC-CAB-01",
			},
		}
		Expect(k8sClient.Create(ctx, sp)).To(Succeed())
		product := &stripev1alpha1.StripeProduct{
			ObjectMeta: metav1.ObjectMeta{Name: stripeProductName(sp), Namespace: pricingNS},
			Spec:       stripev1alpha1.StripeProductSpec{SKU: "DC-CAB-01"},
		}
		Expect(k8sClient.Create(ctx, product)).To(Succeed())
		product.Status.ProductID = "datum_cabling"
		apimeta.SetStatusCondition(&product.Status.Conditions, metav1.Condition{
			Type: stripev1alpha1.StripeProductConditionReady, Status: metav1.ConditionTrue, Reason: "Synced",
		})
		Expect(k8sClient.Status().Update(ctx, product)).To(Succeed())

		inv := invoice("with-sku", "cyberdyne", "cabling-v1", stripev1alpha1.StripeInvoicePurposePreview)
		inv.Spec.Lines[0].Metadata = map[string]string{"billing_entitlement": "cyberdyne"}
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())
		_, err := reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())

		line := stripe.previews[0].Lines[0]
		Expect(line.Description).To(Equal("Rack space"))
		Expect(line.Metadata).To(Equal(map[string]string{"sku": "DC-CAB-01", "billing_entitlement": "cyberdyne"}))
	})

	It("reports a line Stripe doesn't come to", func() {
		readyCustomer("globex")
		readyProduct("rack-v2")
		stripe.result.Lines[0].Amount = 49499
		inv := invoice("mismatch", "globex", "rack-v2", stripev1alpha1.StripeInvoicePurposePreview)
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())
		_, err := reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())

		cond := apimeta.FindStatusCondition(get(inv).Status.Conditions, stripev1alpha1.StripeInvoiceConditionAmountsMatch)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring("494.99"))
	})

	It("waits for the account's Stripe customer and the line's Product", func() {
		inv := invoice("waiting", "initech", "rack-missing", stripev1alpha1.StripeInvoicePurposePreview)
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())

		res, err := reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(stripeInvoiceWaitRequeue))
		Expect(apimeta.FindStatusCondition(get(inv).Status.Conditions, stripev1alpha1.StripeInvoiceConditionReady).Reason).
			To(Equal("WaitingForCustomer"))

		readyCustomer("initech")
		_, err = reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(apimeta.FindStatusCondition(get(inv).Status.Conditions, stripev1alpha1.StripeInvoiceConditionReady).Reason).
			To(Equal("WaitingForProduct"))
		Expect(stripe.previews).To(BeEmpty())
	})

	It("makes a draft for the dashboard, and deletes it with the StripeInvoice", func() {
		readyCustomer("hooli")
		readyProduct("rack-v3")
		inv := invoice("draft", "hooli", "rack-v3", stripev1alpha1.StripeInvoicePurposePreviewDraft)
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())

		res, err := reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically("~", previewDraftLifetime, time.Minute))
		Expect(stripe.drafts).To(HaveLen(1))
		Expect(stripe.drafts[0].Metadata).To(HaveKeyWithValue(stripeinternal.MetadataPurpose, "PreviewDraft"))
		Expect(stripe.drafts[0].Description).To(ContainSubstring("Don't send"))

		got := get(inv)
		Expect(got.Status.InvoiceID).To(Equal("in_draft"))
		Expect(got.Status.DashboardURL).To(Equal("https://dashboard.stripe.com/test/invoices/in_draft"))
		Expect(controllerutil.ContainsFinalizer(got, StripeInvoiceDraftFinalizer)).To(BeTrue())

		Expect(k8sClient.Delete(ctx, got)).To(Succeed())
		_, err = reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(stripe.deleted).To(Equal([]string{"in_draft"}))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(inv), &stripev1alpha1.StripeInvoice{}))).To(BeTrue())
	})

	It("lets a StripeInvoice go when its draft was finalized in Stripe", func() {
		readyCustomer("umbrella")
		readyProduct("rack-v4")
		inv := invoice("finalized", "umbrella", "rack-v4", stripev1alpha1.StripeInvoicePurposePreviewDraft)
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())
		_, err := reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())

		stripe.delErr = errors.Join(stripeinternal.ErrInvoiceNotDraft)
		Expect(k8sClient.Delete(ctx, get(inv))).To(Succeed())
		_, err = reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(inv), &stripev1alpha1.StripeInvoice{}))).To(BeTrue())
	})

	It("keeps a half-made draft's ID so it's deleted later", func() {
		readyCustomer("stark")
		readyProduct("rack-v5")
		stripe.err = errors.New("stripe is down")
		inv := invoice("half-made", "stark", "rack-v5", stripev1alpha1.StripeInvoicePurposePreviewDraft)
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())

		_, err := reconcileInvoice(inv)
		Expect(err).To(HaveOccurred())
		got := get(inv)
		Expect(got.Status.InvoiceID).To(Equal("in_partial"))
		cond := apimeta.FindStatusCondition(got.Status.Conditions, stripev1alpha1.StripeInvoiceConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("StripeError"))
	})

	It("deletes itself once it expires", func() {
		readyCustomer("wayne")
		readyProduct("rack-v6")
		inv := invoice("expired", "wayne", "rack-v6", stripev1alpha1.StripeInvoicePurposePreview)
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())
		now = get(inv).CreationTimestamp.Add(previewLifetime + time.Second)

		_, err := reconcileInvoice(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(stripe.previews).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(inv), &stripev1alpha1.StripeInvoice{}))).To(BeTrue())
	})

	It("refuses lines that can't be invoiced", func() {
		bad := invoice("bad-period", "acme", "rack-v1", stripev1alpha1.StripeInvoicePurposePreview)
		bad.Spec.Lines[0].Period.End = metav1.NewTime(bad.Spec.Lines[0].Period.Start.Add(-time.Hour))
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, bad))).To(BeTrue(), "end before start")

		bad = invoice("bad-tax", "acme", "rack-v1", stripev1alpha1.StripeInvoicePurposePreview)
		bad.Spec.Lines[0].Tax = &stripev1alpha1.StripeInvoiceLineTax{Code: "txcd_10000000", Taxable: ptr.To(false)}
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, bad))).To(BeTrue(), "a code on a line that isn't taxable")

		ok := invoice("immutable", "acme", "rack-v1", stripev1alpha1.StripeInvoicePurposePreview)
		Expect(k8sClient.Create(ctx, ok)).To(Succeed())
		ok.Spec.Lines[0].Quantity = "2"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, ok))).To(BeTrue(), "spec is immutable")
	})
	Context("writing a real invoice", func() {
		realInvoice := func(name, account, pricing string) *stripev1alpha1.StripeInvoice {
			inv := invoice(name, account, pricing, stripev1alpha1.StripeInvoicePurposeInvoice)
			inv.Spec.Lines[0].Tax = nil
			return inv
		}
		condition := func(inv *stripev1alpha1.StripeInvoice, kind string) *metav1.Condition {
			return apimeta.FindStatusCondition(get(inv).Status.Conditions, kind)
		}

		It("keeps a draft in step with its lines, then finalizes it", func() {
			readyCustomer("tyrell")
			readyProduct("rack-w1")
			inv := realInvoice("writer", "tyrell", "rack-w1")
			inv.Spec.Source = &stripev1alpha1.StripeInvoiceSource{System: stripev1alpha1.StripeInvoiceSourceOpenMeter, InvoiceID: "01JDOM"}
			Expect(k8sClient.Create(ctx, inv)).To(Succeed())

			res, err := reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(invoiceRefreshInterval))
			Expect(stripe.drafts).To(HaveLen(1))
			Expect(stripe.drafts[0].Description).To(BeEmpty(), "no preview memo on a real invoice")
			Expect(stripe.drafts[0].CollectionMethod).To(Equal(stripeinternal.CollectionChargeAutomatically))
			Expect(stripe.drafts[0].Metadata).To(HaveKeyWithValue(stripeinternal.MetadataPurpose, "Invoice"))
			Expect(stripe.drafts[0].Metadata).To(HaveKeyWithValue(stripeinternal.MetadataOpenMeterInvoiceID, "01JDOM"))
			got := get(inv)
			Expect(got.Status.InvoiceID).To(Equal("in_draft"))
			Expect(got.Status.State).To(Equal(stripev1alpha1.StripeInvoiceStateDraft))
			Expect(got.Status.ExpiresAt).To(BeNil(), "an invoice never expires")
			Expect(controllerutil.ContainsFinalizer(got, StripeInvoiceDraftFinalizer)).To(BeTrue())
			Expect(condition(inv, stripev1alpha1.StripeInvoiceConditionSynced).Status).To(Equal(metav1.ConditionTrue))
			Expect(stripe.finalized).To(BeEmpty(), "not until asked")

			// Unchanged: not written again.
			_, err = reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(stripe.drafts).To(HaveLen(1))

			// OpenMeter changed the draft: the lines are written again.
			got.Spec.Lines[0].Quantity, got.Spec.Lines[0].Amount = "2", "660.00"
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			stripe.result.Lines[0].Amount = 66000
			_, err = reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(stripe.drafts).To(HaveLen(2))
			Expect(stripe.drafts[1].Lines[0].Quantity.String()).To(Equal("2"))
			Expect(stripe.drafts[1].Revision).NotTo(Equal(stripe.drafts[0].Revision), "a rewrite isn't a retry")
			Expect(condition(inv, stripev1alpha1.StripeInvoiceConditionSynced).Status).To(Equal(metav1.ConditionTrue))

			got = get(inv)
			got.Spec.Finalize = true
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			res, err = reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(stripe.drafts).To(HaveLen(3), "the draft is checked again just before it's finalized")
			Expect(stripe.finalized).To(Equal([]string{"in_draft"}))
			Expect(res.RequeueAfter).To(Equal(invoiceRefreshInterval))
			got = get(inv)
			Expect(got.Status.State).To(Equal(stripev1alpha1.StripeInvoiceStateOpen))
			Expect(got.Status.Number).To(Equal("DATUM-0042"))
			Expect(got.Status.HostedInvoiceURL).To(Equal("https://invoice.stripe.com/i/in_draft"))
			Expect(got.Status.FinalizedAt).NotTo(BeNil())
			Expect(condition(inv, stripev1alpha1.StripeInvoiceConditionFinalized).Status).To(Equal(metav1.ConditionTrue))

			// Stripe reports it paid: read on the next pass, and done.
			stripe.stored.Status, stripe.stored.PaidAt = "paid", time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
			stripe.stored.AmountPaid, stripe.stored.AmountRemaining = stripe.stored.AmountDue, 0
			res, err = reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())
			got = get(inv)
			Expect(got.Status.State).To(Equal(stripev1alpha1.StripeInvoiceStatePaid))
			Expect(got.Status.PaidAt).NotTo(BeNil())
			Expect(got.Status.AmountRemaining).To(Equal("0.00"))
			Expect(stripe.drafts).To(HaveLen(3), "a finalized invoice's lines are never written again")
			Expect(stripe.finalized).To(HaveLen(1))

			// Deleting it leaves the bill in Stripe alone.
			stripe.delErr = errors.Join(stripeinternal.ErrInvoiceNotDraft)
			Expect(k8sClient.Delete(ctx, got)).To(Succeed())
			_, err = reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(inv), &stripev1alpha1.StripeInvoice{}))).To(BeTrue())
		})

		It("sends the invoice, due its net days later, on payment terms", func() {
			readyCustomer("soylent")
			readyProduct("rack-w2")
			ba := &billingv1alpha1.BillingAccount{
				ObjectMeta: metav1.ObjectMeta{Name: "soylent", Namespace: ns},
				Spec:       billingv1alpha1.BillingAccountSpec{CurrencyCode: "USD"},
			}
			Expect(k8sClient.Create(ctx, ba)).To(Succeed())
			ba.Status.PaymentArrangement = &billingv1alpha1.BillingAccountPaymentArrangement{
				Name: "soylent-terms", Type: billingv1alpha1.BillingArrangementTypeInvoice,
				Invoice: &billingv1alpha1.InvoiceArrangementTerms{NetDays: 45},
			}
			Expect(k8sClient.Status().Update(ctx, ba)).To(Succeed())

			inv := realInvoice("terms", "soylent", "rack-w2")
			Expect(k8sClient.Create(ctx, inv)).To(Succeed())
			_, err := reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(stripe.drafts[0].CollectionMethod).To(Equal(stripeinternal.CollectionSendInvoice))
			Expect(stripe.drafts[0].DaysUntilDue).To(Equal(int64(45)))
		})

		It("doesn't finalize an invoice whose amounts differ", func() {
			readyCustomer("oscorp")
			readyProduct("rack-w3")
			stripe.result.Lines[0].Amount = 49499
			inv := realInvoice("differ", "oscorp", "rack-w3")
			inv.Spec.Finalize = true
			Expect(k8sClient.Create(ctx, inv)).To(Succeed())
			_, err := reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			cond := condition(inv, stripev1alpha1.StripeInvoiceConditionSynced)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("AmountsDiffer"))
			Expect(stripe.finalized).To(BeEmpty())
		})

		It("doesn't finalize a draft that changed in Stripe after it was synced", func() {
			readyCustomer("massive")
			readyProduct("rack-w6")
			inv := realInvoice("tampered", "massive", "rack-w6")
			Expect(k8sClient.Create(ctx, inv)).To(Succeed())
			_, err := reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(condition(inv, stripev1alpha1.StripeInvoiceConditionSynced).Status).To(Equal(metav1.ConditionTrue))

			// Someone edits the draft in the Stripe dashboard, and the
			// rewrite can't put it right.
			stripe.result.Lines[0].Amount = 49499
			got := get(inv)
			got.Spec.Finalize = true
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			_, err = reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			Expect(stripe.drafts).To(HaveLen(2))
			Expect(condition(inv, stripev1alpha1.StripeInvoiceConditionSynced).Reason).To(Equal("AmountsDiffer"))
			Expect(stripe.finalized).To(BeEmpty())
		})

		It("says when payment failed, and when its draft was deleted in Stripe", func() {
			readyCustomer("cyber")
			readyProduct("rack-w4")
			inv := realInvoice("failing", "cyber", "rack-w4")
			inv.Spec.Finalize = true
			Expect(k8sClient.Create(ctx, inv)).To(Succeed())
			_, err := reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			stripe.stored.AttemptCount = 1
			stripe.stored.NextPaymentAttempt = time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC)
			_, err = reconcileInvoice(inv)
			Expect(err).NotTo(HaveOccurred())
			cond := condition(inv, stripev1alpha1.StripeInvoiceConditionPaymentFailed)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Message).To(ContainSubstring("2027-01-04"))

			readyCustomer("deleted")
			readyProduct("rack-w5")
			gone := realInvoice("gone", "deleted", "rack-w5")
			Expect(k8sClient.Create(ctx, gone)).To(Succeed())
			_, err = reconcileInvoice(gone)
			Expect(err).NotTo(HaveOccurred())
			stripe.stored = nil
			_, err = reconcileInvoice(gone)
			Expect(err).NotTo(HaveOccurred())
			Expect(get(gone).Status.State).To(Equal(stripev1alpha1.StripeInvoiceStateDeleted))
			Expect(condition(gone, stripev1alpha1.StripeInvoiceConditionSynced).Reason).To(Equal("Deleted"))
		})

		It("lets an invoice's lines change until it's finalized", func() {
			inv := realInvoice("rules", "acme", "rack-v1")
			Expect(k8sClient.Create(ctx, inv)).To(Succeed())
			inv.Spec.Lines[0].Quantity = "2"
			Expect(k8sClient.Update(ctx, inv)).To(Succeed())
			inv.Spec.Finalize = true
			Expect(k8sClient.Update(ctx, inv)).To(Succeed())
			inv.Spec.Finalize = false
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, inv))).To(BeTrue(), "finalize can't be undone")
			inv.Spec.Finalize = true
			inv.Spec.Lines[0].Quantity = "3"
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, inv))).To(BeTrue(), "lines can't change once finalize is set")

			preview := invoice("finalize-preview", "acme", "rack-v1", stripev1alpha1.StripeInvoicePurposePreview)
			preview.Spec.Finalize = true
			Expect(apierrors.IsInvalid(k8sClient.Create(ctx, preview))).To(BeTrue(), "only an Invoice is finalized")
		})
	})
})
