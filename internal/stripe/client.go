// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"errors"
	"fmt"
	"time"

	stripego "github.com/stripe/stripe-go/v81"
	stripeclient "github.com/stripe/stripe-go/v81/client"
)

// Client is the narrow SDK surface the provider needs. Constructed per
// reconcile from a ResolvedConfig — Stripe API keys are configuration,
// not a long-lived process credential.
type Client struct {
	api *stripeclient.API
}

// NewClient builds a Stripe API client.
func NewClient(cfg *ResolvedConfig) *Client {
	api := stripeclient.New(cfg.SecretKey, nil)
	return &Client{api: api}
}

// EnsureCustomer returns the existing customer id when one is recorded;
// otherwise creates a fresh Stripe customer keyed by the billing account
// name for idempotency.
func (c *Client) EnsureCustomer(ctx context.Context, existingID, billingAccountName, email string) (string, error) {
	if existingID != "" {
		return existingID, nil
	}
	params := &stripego.CustomerParams{
		Params: stripego.Params{
			Context: ctx,
			Metadata: map[string]string{
				"billing_account": billingAccountName,
			},
		},
	}
	if email != "" {
		params.Email = stripego.String(email)
	}
	cu, err := c.api.Customers.New(params)
	if err != nil {
		return "", fmt.Errorf("creating Stripe customer: %w", err)
	}
	return cu.ID, nil
}

// SetupIntentResult is the subset of the Stripe SetupIntent fields the
// controller needs.
type SetupIntentResult struct {
	ID           string
	ClientSecret string
	Status       string
	ExpiresAt    *time.Time
}

// CreateSetupIntent creates a card-only off-session SetupIntent for the
// supplied customer.
func (c *Client) CreateSetupIntent(ctx context.Context, customerID, stripePaymentMethodNamespace, stripePaymentMethodName string) (*SetupIntentResult, error) {
	if customerID == "" {
		return nil, errors.New("CreateSetupIntent requires a customer id")
	}
	params := &stripego.SetupIntentParams{
		Params: stripego.Params{
			Context: ctx,
			Metadata: map[string]string{
				"stripe_payment_method_namespace": stripePaymentMethodNamespace,
				"stripe_payment_method_name":      stripePaymentMethodName,
			},
		},
		Customer:           stripego.String(customerID),
		Usage:              stripego.String(string(stripego.SetupIntentUsageOffSession)),
		PaymentMethodTypes: stripego.StringSlice([]string{"card"}),
	}
	si, err := c.api.SetupIntents.New(params)
	if err != nil {
		return nil, fmt.Errorf("creating Stripe SetupIntent: %w", err)
	}
	result := &SetupIntentResult{
		ID:           si.ID,
		ClientSecret: si.ClientSecret,
		Status:       string(si.Status),
	}
	return result, nil
}

// PaymentMethodDetails is the subset of the Stripe PaymentMethod fields
// the provider records.
type PaymentMethodDetails struct {
	ID        string
	Type      string
	Brand     string
	Last4     string
	BIN       string
	Country   string
	ExpMonth  int32
	ExpYear   int32
	AVSResult string
	CVCResult string
}

// DetachPaymentMethod detaches a PaymentMethod from its Stripe customer.
// Idempotent: a PaymentMethod that has already been detached (or was
// never attached) returns nil so callers can treat the failure mode as
// "already cleaned up".
func (c *Client) DetachPaymentMethod(ctx context.Context, paymentMethodID string) error {
	if paymentMethodID == "" {
		return nil
	}
	if _, err := c.api.PaymentMethods.Detach(paymentMethodID, &stripego.PaymentMethodDetachParams{
		Params: stripego.Params{Context: ctx},
	}); err != nil {
		if stripeErr, ok := err.(*stripego.Error); ok {
			// resource_missing — Stripe has no record of the PM (already
			// detached, deleted, never created). Idempotent path.
			if stripeErr.Code == stripego.ErrorCodeResourceMissing {
				return nil
			}
		}
		return fmt.Errorf("detaching PaymentMethod %q: %w", paymentMethodID, err)
	}
	return nil
}

// CancelSetupIntent cancels a SetupIntent. Idempotent on the
// resource_missing and already-canceled / already-succeeded states.
func (c *Client) CancelSetupIntent(ctx context.Context, setupIntentID string) error {
	if setupIntentID == "" {
		return nil
	}
	if _, err := c.api.SetupIntents.Cancel(setupIntentID, &stripego.SetupIntentCancelParams{
		Params: stripego.Params{Context: ctx},
	}); err != nil {
		if stripeErr, ok := err.(*stripego.Error); ok {
			switch stripeErr.Code {
			case stripego.ErrorCodeResourceMissing,
				stripego.ErrorCodeSetupIntentUnexpectedState:
				return nil
			}
		}
		return fmt.Errorf("canceling SetupIntent %q: %w", setupIntentID, err)
	}
	return nil
}

// RetrievePaymentMethod fetches a confirmed PaymentMethod.
func (c *Client) RetrievePaymentMethod(ctx context.Context, paymentMethodID string) (*PaymentMethodDetails, error) {
	pm, err := c.api.PaymentMethods.Get(paymentMethodID, &stripego.PaymentMethodParams{
		Params: stripego.Params{Context: ctx},
	})
	if err != nil {
		return nil, fmt.Errorf("retrieving PaymentMethod %q: %w", paymentMethodID, err)
	}
	out := &PaymentMethodDetails{ID: pm.ID, Type: string(pm.Type)}
	if pm.Card != nil {
		out.Brand = string(pm.Card.Brand)
		out.Last4 = pm.Card.Last4
		out.Country = pm.Card.Country
		out.ExpMonth = int32(pm.Card.ExpMonth)
		out.ExpYear = int32(pm.Card.ExpYear)
		out.BIN = pm.Card.IIN
		if pm.Card.Checks != nil {
			out.AVSResult = firstNonEmpty(string(pm.Card.Checks.AddressLine1Check), string(pm.Card.Checks.AddressPostalCodeCheck))
			out.CVCResult = string(pm.Card.Checks.CVCCheck)
		}
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
