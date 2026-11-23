// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

func TestCustomerDescription(t *testing.T) {
	account := func(display string) *billingv1alpha1.BillingAccount {
		ba := &billingv1alpha1.BillingAccount{ObjectMeta: metav1.ObjectMeta{Name: "ba-acme-x1y2z3"}}
		if display != "" {
			ba.Annotations = map[string]string{"kubernetes.io/display-name": display}
		}
		return ba
	}
	for display, want := range map[string]string{
		"Acme":           "Acme · ba-acme-x1y2z3",
		"":               "ba-acme-x1y2z3",
		"  ":             "ba-acme-x1y2z3",
		"ba-acme-x1y2z3": "ba-acme-x1y2z3",
	} {
		if got := customerDescription(account(display)); got != want {
			t.Errorf("customerDescription(%q) = %q, want %q", display, got, want)
		}
	}
}
