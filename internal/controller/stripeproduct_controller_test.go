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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeProductEnsurer records the Products the reconciler asks Stripe for.
type fakeProductEnsurer struct {
	mu    sync.Mutex
	calls []stripeinternal.ProductDetails
	err   error
}

func (f *fakeProductEnsurer) EnsureProduct(_ context.Context, d stripeinternal.ProductDetails) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, d)
	return f.err
}

func (f *fakeProductEnsurer) last() stripeinternal.ProductDetails {
	f.mu.Lock()
	defer f.mu.Unlock()
	Expect(f.calls).NotTo(BeEmpty())
	return f.calls[len(f.calls)-1]
}

var _ = Describe("StripeProduct", func() {
	const ns = "stripe-products"
	var stripe *fakeProductEnsurer

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		stripe = &fakeProductEnsurer{}
	})

	fee := func(name, sku, basedOn, displayName string, tax *billingv1alpha1.PricingTax) *billingv1alpha1.ServicePricing {
		return &billingv1alpha1.ServicePricing{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeRecurring, Currency: "USD", Amount: "300.00",
				Interval: billingv1alpha1.ChargeIntervalMonthly, SKU: sku, BasedOn: basedOn,
				DisplayName: displayName, Tax: tax,
			},
		}
	}

	// sync runs both reconcilers for the pricing, as its events would, and
	// returns its StripeProduct.
	sync := func(sp *billingv1alpha1.ServicePricing) *stripev1alpha1.StripeProduct {
		key := types.NamespacedName{Namespace: ns, Name: stripeProductName(sp)}
		pricings := &ServicePricingProductReconciler{Client: k8sClient}
		_, err := pricings.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sp)})
		Expect(err).NotTo(HaveOccurred())

		products := &StripeProductReconciler{Client: k8sClient, stripeClientFactory: func() stripeProductEnsurer { return stripe }}
		_, _ = products.Reconcile(ctx, reconcile.Request{NamespacedName: key})

		var product stripev1alpha1.StripeProduct
		Expect(k8sClient.Get(ctx, key, &product)).To(Succeed())
		return &product
	}

	It("makes one Product per SKU, named and taxed by its list pricing", func() {
		list := fee("cabling-v1", "DC-CAB-01", "", "Structured cabling", &billingv1alpha1.PricingTax{Code: "txcd_20030000"})
		customer := fee("acme-cabling", "DC-CAB-01", "cabling-v1", "Cabling (Acme)", &billingv1alpha1.PricingTax{Code: "txcd_20030000"})
		Expect(k8sClient.Create(ctx, list)).To(Succeed())
		Expect(k8sClient.Create(ctx, customer)).To(Succeed())

		product := sync(list)
		Expect(sync(customer).Name).To(Equal(product.Name), "a customer pricing shares its list pricing's Product")
		Expect(product.Spec.SKU).To(Equal("DC-CAB-01"))

		sent := stripe.last()
		Expect(sent.ID).To(Equal(stripeProductID(product)))
		Expect(sent.Name).To(Equal("Structured cabling"))
		Expect(sent.TaxCode).To(Equal("txcd_20030000"))
		Expect(sent.Active).To(BeTrue())
		Expect(sent.Metadata).To(HaveKeyWithValue("datum_sku", "DC-CAB-01"))

		product = sync(customer)
		Expect(product.Status.ProductID).To(Equal(sent.ID))
		Expect(product.Status.Pricings).To(Equal([]string{"acme-cabling", "cabling-v1"}))
		Expect(apimeta.IsStatusConditionTrue(product.Status.Conditions, stripev1alpha1.StripeProductConditionReady)).To(BeTrue())
		Expect(apimeta.IsStatusConditionFalse(product.Status.Conditions, stripev1alpha1.StripeProductConditionTaxCodeConflict)).To(BeTrue())
	})

	It("gives a list pricing without a SKU a Product of its own", func() {
		setup := fee("setup-v1", "", "", "", nil)
		Expect(k8sClient.Create(ctx, setup)).To(Succeed())

		product := sync(setup)
		Expect(product.Name).To(Equal("pricing-setup-v1"))
		Expect(product.Spec.PricingRef).NotTo(BeNil())
		Expect(product.Spec.PricingRef.Name).To(Equal("setup-v1"))
		Expect(stripe.last().Name).To(Equal("setup-v1"))
		Expect(stripe.last().TaxCode).To(BeEmpty())
	})

	It("reports list pricings taxed differently", func() {
		older := fee("power-v1", "PWR-01", "", "Power", nil)
		Expect(k8sClient.Create(ctx, older)).To(Succeed())
		sync(older)
		newer := fee("power-v2", "PWR-01", "", "Power commit", &billingv1alpha1.PricingTax{Code: "txcd_10000000"})
		Expect(k8sClient.Create(ctx, newer)).To(Succeed())

		product := sync(newer)
		// Both were created within the same second, so the later name breaks
		// the tie, as it would for a new price.
		Expect(stripe.last().TaxCode).To(Equal("txcd_10000000"))
		cond := apimeta.FindStatusCondition(product.Status.Conditions, stripev1alpha1.StripeProductConditionTaxCodeConflict)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring("power-v1"))
	})

	It("archives the Product once no pricing stands for it", func() {
		gone := fee("rack-v1", "RACK-01", "", "Rack", nil)
		Expect(k8sClient.Create(ctx, gone)).To(Succeed())
		Expect(sync(gone).Status.Active).To(BeTrue())

		Expect(k8sClient.Delete(ctx, gone)).To(Succeed())
		products := &StripeProductReconciler{Client: k8sClient, stripeClientFactory: func() stripeProductEnsurer { return stripe }}
		key := types.NamespacedName{Namespace: ns, Name: stripeProductName(gone)}
		_, err := products.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(stripe.last().Active).To(BeFalse())
		Expect(stripe.last().Name).To(Equal("Rack"))
		var product stripev1alpha1.StripeProduct
		Expect(k8sClient.Get(ctx, key, &product)).To(Succeed())
		Expect(product.Status.Active).To(BeFalse())
		Expect(product.Status.Pricings).To(BeEmpty())
	})

	It("records a Stripe failure and retries", func() {
		sp := fee("xconn-v1", "XC-01", "", "Cross connect", nil)
		Expect(k8sClient.Create(ctx, sp)).To(Succeed())
		stripe.err = errors.New("stripe is down")

		pricings := &ServicePricingProductReconciler{Client: k8sClient}
		_, err := pricings.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sp)})
		Expect(err).NotTo(HaveOccurred())
		products := &StripeProductReconciler{Client: k8sClient, stripeClientFactory: func() stripeProductEnsurer { return stripe }}
		key := types.NamespacedName{Namespace: ns, Name: stripeProductName(sp)}
		_, err = products.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())

		var product stripev1alpha1.StripeProduct
		Expect(k8sClient.Get(ctx, key, &product)).To(Succeed())
		cond := apimeta.FindStatusCondition(product.Status.Conditions, stripev1alpha1.StripeProductConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring("stripe is down"))
		Expect(product.Status.ProductID).To(BeEmpty())
	})

	It("refuses a spec with neither a SKU nor a pricing", func() {
		err := k8sClient.Create(ctx, &stripev1alpha1.StripeProduct{ObjectMeta: metav1.ObjectMeta{Name: "neither", Namespace: ns}})
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})
})
