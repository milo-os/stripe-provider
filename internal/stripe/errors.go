// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import "fmt"

// TaxIDError is returned by EnsureCustomer when the Customer record was
// created or updated successfully on Stripe but the follow-up reconcile
// of tax_ids against that Customer failed. Callers should treat this as
// non-fatal: the returned customer ID is valid and usable for downstream
// operations (SetupIntent, attaching PaymentMethods, etc.), and the
// tax-ID failure should surface to the user via a status condition
// rather than blocking the whole payment flow.
//
// Detect via errors.As:
//
//	var taxErr *stripe.TaxIDError
//	if errors.As(err, &taxErr) {
//	    // customerID is valid; record condition and continue.
//	}
type TaxIDError struct {
	Underlying error
}

func (e *TaxIDError) Error() string {
	return fmt.Sprintf("reconciling Stripe tax IDs: %v", e.Underlying)
}

func (e *TaxIDError) Unwrap() error { return e.Underlying }
