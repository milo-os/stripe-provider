// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Invoice numbers", func() {
	const (
		ns         = "invoice-numbers"
		pricingNS  = "invoice-numbers-pricing"
		configName = "invoice-numbers"
		pricing    = "rack-n1"
	)
	var (
		stripe    *fakeInvoicer
		customers *StripeCustomers
		recorder  *record.FakeRecorder
	)

	reconcilerWith := func(c *StripeCustomers) *StripeInvoiceReconciler {
		return &StripeInvoiceReconciler{
			Client:              k8sClient,
			Recorder:            recorder,
			ProviderConfigName:  configName,
			Customers:           c,
			stripeClientFactory: func() stripeInvoicer { return stripe },
		}
	}

	// account gives the account a StripeCustomer with a Stripe customer,
	// and the number and InvoicePrefixSet condition given, if any.
	account := func(name string, number int64, prefixSet bool) {
		sc := &stripev1alpha1.StripeCustomer{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       stripev1alpha1.StripeCustomerSpec{BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: name}},
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		sc.Status.StripeCustomerID = "cus_" + name
		sc.Status.CustomerNumber = number
		if prefixSet {
			apimeta.SetStatusCondition(&sc.Status.Conditions, metav1.Condition{
				Type: stripev1alpha1.StripeCustomerConditionInvoicePrefixSet, Status: metav1.ConditionTrue, Reason: "PrefixSet",
			})
		}
		Expect(k8sClient.Status().Update(ctx, sc)).To(Succeed())
	}
	customer := func(name string) *stripev1alpha1.StripeCustomer {
		sc := &stripev1alpha1.StripeCustomer{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sc)).To(Succeed())
		return sc
	}
	prefixSet := func(name string) bool {
		return apimeta.IsStatusConditionTrue(customer(name).Status.Conditions, stripev1alpha1.StripeCustomerConditionInvoicePrefixSet)
	}
	config := func() *stripev1alpha1.StripeProviderConfig {
		cfg := &stripev1alpha1.StripeProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configName}, cfg)).To(Succeed())
		return cfg
	}
	setCounter := func(n int64) {
		cfg := config()
		cfg.Status.LastCustomerNumber = n
		Expect(k8sClient.Status().Update(ctx, cfg)).To(Succeed())
	}

	// finalize creates an invoice for the account, asked to be finalized,
	// and reconciles it once with c.
	finalize := func(c *StripeCustomers, name, acct string) (*stripev1alpha1.StripeInvoice, error) {
		inv := &stripev1alpha1.StripeInvoice{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: stripev1alpha1.StripeInvoiceSpec{
				BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: acct},
				Purpose:           stripev1alpha1.StripeInvoicePurposeInvoice,
				Finalize:          true,
				Lines: []stripev1alpha1.StripeInvoiceLine{{
					Key: "rack", Description: "Rack space",
					ServicePricingRef: stripev1alpha1.ServicePricingRef{Namespace: pricingNS, Name: pricing},
					Quantity:          "1.5", UnitAmount: "330.00", Amount: "495.00",
					Period: stripev1alpha1.StripeInvoiceLinePeriod{
						Start: metav1.Date(2026, 12, 15, 0, 0, 0, 0, metav1.Now().Location()),
						End:   metav1.Date(2027, 1, 14, 0, 0, 0, 0, metav1.Now().Location()),
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())
		_, err := reconcilerWith(c).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(inv)})
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inv), inv)).To(Succeed())
		return inv, err
	}
	finalized := func(inv *stripev1alpha1.StripeInvoice) *metav1.Condition {
		return apimeta.FindStatusCondition(inv.Status.Conditions, stripev1alpha1.StripeInvoiceConditionFinalized)
	}

	BeforeEach(func() {
		for _, n := range []string{ns, pricingNS} {
			err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		}
		sp := &billingv1alpha1.ServicePricing{
			ObjectMeta: metav1.ObjectMeta{Name: pricing, Namespace: pricingNS},
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeRecurring, Currency: "USD", Amount: "330.00",
				Interval: billingv1alpha1.ChargeIntervalMonthly,
			},
		}
		if err := k8sClient.Create(ctx, sp); err == nil {
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
		} else {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}

		Expect(k8sClient.Create(ctx, &stripev1alpha1.StripeProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: configName},
			Spec: stripev1alpha1.StripeProviderConfigSpec{
				PublishableKey: "pk_test_numbers",
				InvoiceNumbers: &stripev1alpha1.InvoiceNumbersConfig{FirstCustomerNumber: 1001},
			},
		})).To(Succeed())

		stripe = &fakeInvoicer{result: stripeinternal.InvoiceResult{
			Subtotal: 49500, Total: 49500, AmountDue: 49500,
			Lines: []stripeinternal.LineResult{{Key: "rack", Amount: 49500}},
		}}
		customers = NewStripeCustomers(k8sClient, k8sClient)
		recorder = record.NewFakeRecorder(10)
	})

	AfterEach(func() {
		// Numbers are counted across every StripeCustomer, so each test
		// starts from none.
		Expect(k8sClient.DeleteAllOf(ctx, &stripev1alpha1.StripeCustomer{}, client.InNamespace(ns))).To(Succeed())
		Expect(k8sClient.Delete(ctx, &stripev1alpha1.StripeProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: configName}})).To(Succeed())
	})

	It("hands out numbers in order from firstCustomerNumber and sets them as prefixes", func() {
		account("first", 0, false)
		account("second", 0, false)

		inv, err := finalize(customers, "first-1", "first")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalized(inv).Status).To(Equal(metav1.ConditionTrue))
		inv, err = finalize(customers, "second-1", "second")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalized(inv).Status).To(Equal(metav1.ConditionTrue))

		Expect(customer("first").Status.CustomerNumber).To(Equal(int64(1001)))
		Expect(customer("second").Status.CustomerNumber).To(Equal(int64(1002)))
		Expect(prefixSet("first")).To(BeTrue())
		Expect(prefixSet("second")).To(BeTrue())
		Expect(stripe.prefixSets).To(Equal([]string{"cus_first=1001", "cus_second=1002"}))
		Expect(config().Status.LastCustomerNumber).To(Equal(int64(1002)))

		// A second invoice for the same account keeps its number and
		// sends Stripe nothing.
		_, err = finalize(customers, "first-2", "first")
		Expect(err).NotTo(HaveOccurred())
		Expect(customer("first").Status.CustomerNumber).To(Equal(int64(1001)))
		Expect(stripe.prefixSets).To(HaveLen(2))
		Expect(stripe.finalized).To(HaveLen(3))
	})

	It("leaves prefixes alone when invoiceNumbers is unset", func() {
		cfg := config()
		cfg.Spec.InvoiceNumbers = nil
		Expect(k8sClient.Update(ctx, cfg)).To(Succeed())
		account("plain", 0, false)

		inv, err := finalize(customers, "plain-1", "plain")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalized(inv).Status).To(Equal(metav1.ConditionTrue))
		Expect(customer("plain").Status.CustomerNumber).To(BeZero())
		Expect(stripe.prefixSets).To(BeEmpty())
	})

	It("retries a conflicting counter write", func() {
		account("racer", 0, false)
		bumped := false
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		racing := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*stripev1alpha1.StripeProviderConfig); ok && !bumped {
					// Another writer hands out 1001 first.
					bumped = true
					setCounter(1001)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		})

		_, err = finalize(NewStripeCustomers(racing, k8sClient), "racer-1", "racer")
		Expect(err).NotTo(HaveOccurred())
		Expect(bumped).To(BeTrue())
		Expect(customer("racer").Status.CustomerNumber).To(Equal(int64(1002)))
		Expect(config().Status.LastCustomerNumber).To(Equal(int64(1002)))
	})

	It("raises a lost counter to the highest number a StripeCustomer holds", func() {
		account("older", 1500, true)
		account("newer", 0, false)

		_, err := finalize(customers, "newer-1", "newer")
		Expect(err).NotTo(HaveOccurred())
		Expect(customer("newer").Status.CustomerNumber).To(Equal(int64(1501)))
	})

	It("never reuses a number after a restart part-way through handing one out", func() {
		// The counter moved to 1001, then the process stopped before the
		// number was recorded on a StripeCustomer.
		setCounter(1001)
		account("after-restart", 0, false)

		_, err := finalize(customers, "after-restart-1", "after-restart")
		Expect(err).NotTo(HaveOccurred())
		Expect(customer("after-restart").Status.CustomerNumber).To(Equal(int64(1002)))
	})

	It("sets the prefix of a number recorded before a crash, before finalizing", func() {
		// Steps 1 to 3 happened; Stripe still has the random prefix.
		setCounter(1001)
		account("crashed", 1001, false)
		stripe.prefixes = map[string]string{"cus_crashed": "K7QX2M4P"}

		inv, err := finalize(customers, "crashed-1", "crashed")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalized(inv).Status).To(Equal(metav1.ConditionTrue))
		Expect(stripe.prefixSets).To(Equal([]string{"cus_crashed=1001"}))
		Expect(customer("crashed").Status.CustomerNumber).To(Equal(int64(1001)))
		Expect(prefixSet("crashed")).To(BeTrue())
		Expect(config().Status.LastCustomerNumber).To(Equal(int64(1001)), "no new number")
	})

	It("records a prefix Stripe set whose reply was lost, without writing it again", func() {
		account("lost-reply", 1001, false)
		stripe.prefixes = map[string]string{"cus_lost-reply": "1001"}

		_, err := finalize(customers, "lost-reply-1", "lost-reply")
		Expect(err).NotTo(HaveOccurred())
		Expect(stripe.prefixSets).To(BeEmpty())
		Expect(prefixSet("lost-reply")).To(BeTrue())
	})

	It("puts back a prefix changed in the Stripe dashboard", func() {
		account("edited", 1001, true)
		stripe.prefixes = map[string]string{"cus_edited": "STAFF1"}

		inv, err := finalize(customers, "edited-1", "edited")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalized(inv).Status).To(Equal(metav1.ConditionTrue))
		Expect(stripe.prefixSets).To(Equal([]string{"cus_edited=1001"}))
	})

	It("replaces a refused number that no invoice carries yet", func() {
		account("refused", 0, false)
		stripe.taken = map[string]bool{"1001": true}

		inv, err := finalize(customers, "refused-1", "refused")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalized(inv).Status).To(Equal(metav1.ConditionTrue))
		Expect(customer("refused").Status.CustomerNumber).To(Equal(int64(1002)))
		Expect(stripe.prefixSets).To(Equal([]string{"cus_refused=1002"}))
		Expect(recorder.Events).To(Receive(ContainSubstring("CustomerNumberRefused")))
	})

	It("holds the finalize when Stripe refuses a number the customer's invoices carry", func() {
		account("held", 1001, true)
		stripe.prefixes = map[string]string{"cus_held": "STAFF1"}
		stripe.taken = map[string]bool{"1001": true}

		inv, err := finalize(customers, "held-1", "held")
		Expect(errors.Is(err, errPrefixTakenAfterUse)).To(BeTrue())
		Expect(stripe.finalized).To(BeEmpty())
		Expect(finalized(inv).Status).To(Equal(metav1.ConditionFalse))
		Expect(finalized(inv).Reason).To(Equal("NumberingFailed"))
		Expect(customer("held").Status.CustomerNumber).To(Equal(int64(1001)), "the number stays")
	})

	It("doesn't finalize when the prefix can't be read", func() {
		account("unreadable", 0, false)
		stripe.prefixErr = errors.New("stripe is down")

		inv, err := finalize(customers, "unreadable-1", "unreadable")
		Expect(err).To(HaveOccurred())
		Expect(stripe.finalized).To(BeEmpty())
		Expect(finalized(inv).Reason).To(Equal("NumberingFailed"))
	})

	It("gives invoices finalized together, by two processes, different numbers", func() {
		account("left", 0, false)
		account("right", 0, false)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, acct := range []string{"left", "right"} {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				// Each with its own StripeCustomers, as two processes would be.
				_, errs[i] = finalize(NewStripeCustomers(k8sClient, k8sClient), acct+"-1", acct)
			}()
		}
		wg.Wait()
		Expect(errs).To(HaveEach(BeNil()))
		Expect([]int64{customer("left").Status.CustomerNumber, customer("right").Status.CustomerNumber}).
			To(ConsistOf(int64(1001), int64(1002)))
		Expect(config().Status.LastCustomerNumber).To(Equal(int64(1002)))
	})
})
