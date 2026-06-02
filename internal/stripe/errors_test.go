// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"errors"
	"fmt"
	"testing"
)

func TestTaxIDError_AsAndUnwrap(t *testing.T) {
	root := errors.New("Invalid value for gb_vat.")
	// Wrap as a controller would receive it from EnsureCustomer: a
	// TaxIDError wrapping the root cause, possibly nested under
	// additional fmt.Errorf layers added by callers.
	taxErr := &TaxIDError{Underlying: fmt.Errorf("creating tax_id gb_vat=GB12323234: %w", root)}
	wrapped := fmt.Errorf("ensuring Stripe customer: %w", taxErr)

	var got *TaxIDError
	if !errors.As(wrapped, &got) {
		t.Fatalf("errors.As must detect *TaxIDError through fmt.Errorf wrap; got %T", wrapped)
	}
	if got != taxErr {
		t.Fatalf("errors.As returned a different pointer than the original; want %p, got %p", taxErr, got)
	}
	if !errors.Is(wrapped, root) {
		t.Fatalf("errors.Is must reach the root cause via Unwrap chain")
	}
	if got.Error() == "" {
		t.Fatal("Error() must return a non-empty message")
	}
}

func TestTaxIDError_NotMistakenForOtherErrors(t *testing.T) {
	// A plain Stripe error from a non-tax path must not match *TaxIDError.
	other := fmt.Errorf("creating Stripe customer: api error")
	var got *TaxIDError
	if errors.As(other, &got) {
		t.Fatalf("errors.As must not match non-TaxIDError values")
	}
}
