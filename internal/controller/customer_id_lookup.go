// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// findDefaultStripePaymentMethodID resolves the Stripe `pm_…` ID for
// the PaymentMethod the BillingAccount has nominated as its default,
// when that chain is complete:
//
//   - the BA has `spec.defaultPaymentMethodRef.name` set,
//   - a `StripePaymentMethod` exists with that name (the watcher names
//     SPMs to match their parent PaymentMethod),
//   - the SPM has been confirmed and carries
//     `status.stripePaymentMethodId`.
//
// Returns "" in every other case (no default, SPM not yet created,
// SetupIntent not yet confirmed, parent PaymentMethod deleted out from
// under the BA ref, etc.). The reconciler hands that empty string
// through to Stripe, which clears
// Customer.invoice_settings.default_payment_method — the right
// behaviour every time the BA's nominated default isn't actually
// usable upstream yet.
func findDefaultStripePaymentMethodID(
	ctx context.Context,
	c client.Client,
	ba *billingv1alpha1.BillingAccount,
) (string, error) {
	ref := ba.Spec.DefaultPaymentMethodRef
	if ref == nil || ref.Name == "" {
		return "", nil
	}
	var spm stripev1alpha1.StripePaymentMethod
	key := client.ObjectKey{Namespace: ba.Namespace, Name: ref.Name}
	if err := c.Get(ctx, key, &spm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("getting StripePaymentMethod %q for default ref: %w", ref.Name, err)
	}
	return spm.Status.StripePaymentMethodID, nil
}

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
