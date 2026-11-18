// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

func rulesPricing(name, sku, basedOn, displayName string, created time.Time, tax *billingv1alpha1.PricingTax) billingv1alpha1.ServicePricing {
	return billingv1alpha1.ServicePricing{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "milo-system", CreationTimestamp: metav1.NewTime(created)},
		Spec: billingv1alpha1.ServicePricingSpec{
			ChargeType: billingv1alpha1.ChargeTypeRecurring, SKU: sku, BasedOn: basedOn,
			DisplayName: displayName, Tax: tax,
		},
	}
}

func TestStripeProductName(t *testing.T) {
	sku := func(s string) *billingv1alpha1.ServicePricing {
		return &billingv1alpha1.ServicePricing{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: billingv1alpha1.ServicePricingSpec{SKU: s}}
	}
	if a, b := stripeProductName(sku("DC-CAB-01")), stripeProductName(sku("dc-cab-01")); a == b {
		t.Errorf("SKUs differing only in case share a name: %s", a)
	}
	if a, b := stripeProductName(sku("DC_CAB_01")), stripeProductName(sku("DC-CAB-01")); a == b {
		t.Errorf("SKUs differing only in punctuation share a name: %s", a)
	}
	if got := stripeProductName(sku("DC-CAB-01")); !strings.HasPrefix(got, "sku-dc-cab-01-") {
		t.Errorf("name = %q", got)
	}

	list := &billingv1alpha1.ServicePricing{ObjectMeta: metav1.ObjectMeta{Name: "setup-v1"}}
	customer := &billingv1alpha1.ServicePricing{ObjectMeta: metav1.ObjectMeta{Name: "contract-acme-fee-1"},
		Spec: billingv1alpha1.ServicePricingSpec{BasedOn: "setup-v1"}}
	if a, b := stripeProductName(list), stripeProductName(customer); a != "pricing-setup-v1" || a != b {
		t.Errorf("a customer pricing without a SKU should share its list pricing's product: %s, %s", a, b)
	}

	long := &billingv1alpha1.ServicePricing{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 253)}}
	if got := stripeProductName(long); len(got) > maxNameLength {
		t.Errorf("name is %d long", len(got))
	}
}

func TestDesiredProduct(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	product := &stripev1alpha1.StripeProduct{ObjectMeta: metav1.ObjectMeta{Name: stripeProductName(&billingv1alpha1.ServicePricing{
		Spec: billingv1alpha1.ServicePricingSpec{SKU: "DC-CAB-01"}})}}
	space := &billingv1alpha1.PricingTax{Code: "txcd_20030000"}

	t.Run("the newest list pricing names and taxes it", func(t *testing.T) {
		got := desiredProduct(product, []billingv1alpha1.ServicePricing{
			rulesPricing("cabling-v1", "DC-CAB-01", "", "Cabling", day(1), nil),
			rulesPricing("cabling-v2", "DC-CAB-01", "", "Structured cabling", day(10), space),
			rulesPricing("acme-cabling", "DC-CAB-01", "cabling-v2", "Cabling (Acme)", day(15), space),
			rulesPricing("other", "DC-XC-01", "", "Cross connect", day(20), nil),
		})
		if got.name != "Structured cabling" || got.taxCode != "txcd_20030000" || !got.active {
			t.Errorf("got %+v", got)
		}
		if want := []string{"acme-cabling", "cabling-v1", "cabling-v2"}; !slices.Equal(got.pricings, want) {
			t.Errorf("pricings = %v, want %v", got.pricings, want)
		}
		// cabling-v1 predates tax codes: it's reported, not obeyed.
		if want := []string{"cabling-v1"}; !slices.Equal(got.conflicts, want) {
			t.Errorf("conflicts = %v, want %v", got.conflicts, want)
		}
	})

	t.Run("a pricing that isn't taxable is sold as nontaxable", func(t *testing.T) {
		got := desiredProduct(product, []billingv1alpha1.ServicePricing{
			rulesPricing("deposit", "DC-CAB-01", "", "", day(1), &billingv1alpha1.PricingTax{Taxable: ptr.To(false)}),
		})
		if got.name != "deposit" || got.taxCode != StripeNontaxableTaxCode {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("with only customer pricings left, the newest names it", func(t *testing.T) {
		got := desiredProduct(product, []billingv1alpha1.ServicePricing{
			rulesPricing("acme", "DC-CAB-01", "gone", "Cabling (Acme)", day(1), space),
			rulesPricing("globex", "DC-CAB-01", "gone", "Cabling (Globex)", day(2), space),
		})
		if got.name != "Cabling (Globex)" || !got.active || len(got.conflicts) != 0 {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("with no pricings left it's archived as it was", func(t *testing.T) {
		archived := product.DeepCopy()
		archived.Status.Name, archived.Status.TaxCode = "Structured cabling", "txcd_20030000"
		got := desiredProduct(archived, nil)
		if got.active || got.name != "Structured cabling" || got.taxCode != "txcd_20030000" {
			t.Errorf("got %+v", got)
		}
	})
}

func TestStripeProductID(t *testing.T) {
	a := &stripev1alpha1.StripeProduct{ObjectMeta: metav1.ObjectMeta{Namespace: "milo-system", Name: "sku-a"}}
	b := &stripev1alpha1.StripeProduct{ObjectMeta: metav1.ObjectMeta{Namespace: "milo-system", Name: "sku-b"}}
	if stripeProductID(a) == stripeProductID(b) || stripeProductID(a) != stripeProductID(a.DeepCopy()) {
		t.Errorf("ids should be stable and distinct: %s %s", stripeProductID(a), stripeProductID(b))
	}
	if !strings.HasPrefix(stripeProductID(a), "datum_") {
		t.Errorf("id = %s", stripeProductID(a))
	}
}
