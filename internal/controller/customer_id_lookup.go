// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// findExistingCustomerID walks the StripePaymentMethod resources in the
// given namespace looking for any sibling whose parent PaymentMethod is
// bound to the same BillingAccount as the caller and that has already
// recorded a Stripe customer ID on its status. Returns the customer ID
// of the first such sibling, or "" when none is found.
//
// This is the local-state half of stripe-customer deduplication: each
// PaymentMethod the user adds against the same BillingAccount should
// attach to the same Stripe Customer, but the StripePaymentMethod
// reconciler only sees its own resource. Without this lookup every
// PaymentMethod minted a new Customer, leaving an orphan per attempt.
//
// The function is intentionally tolerant of partially-set state: a
// PaymentMethod whose `billingAccountRef` is missing or that doesn't
// match `billingAccountName` is skipped; a StripePaymentMethod with no
// customer ID on status is skipped; PaymentMethod resources that have
// been deleted underneath their child are skipped. Only deliberate
// matches contribute to the returned ID.
func findExistingCustomerID(
	ctx context.Context,
	c client.Client,
	namespace string,
	billingAccountName string,
) (string, error) {
	var list stripev1alpha1.StripePaymentMethodList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("listing StripePaymentMethods in %q: %w", namespace, err)
	}
	for i := range list.Items {
		spm := &list.Items[i]
		if spm.Status.StripeCustomerID == "" {
			continue
		}
		if spm.Spec.PaymentMethodRef.Name == "" {
			continue
		}
		var pm billingv1alpha1.PaymentMethod
		key := client.ObjectKey{Namespace: spm.Namespace, Name: spm.Spec.PaymentMethodRef.Name}
		if err := c.Get(ctx, key, &pm); err != nil {
			// PaymentMethod gone (or transient API error) — skip this
			// sibling rather than fail the caller's reconcile.
			continue
		}
		if pm.Spec.BillingAccountRef.Name != billingAccountName {
			continue
		}
		return spm.Status.StripeCustomerID, nil
	}
	return "", nil
}
