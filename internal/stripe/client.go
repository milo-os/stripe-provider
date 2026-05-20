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

// CustomerDetails carries the BillingAccount-derived fields that
// stripe-provider stamps onto a Stripe Customer on every reconcile.
// All fields are optional from the SDK's perspective; the reconciler
// passes empty values to indicate "leave unset".
type CustomerDetails struct {
	// Name is the full display name (typically firstName + " " + lastName).
	Name string
	// Email is the invoice recipient. Falls back to the
	// BillingAccount contact email when invoiceEmail is unset.
	Email string
	// Address is the postal billing address. May be nil when the
	// BillingAccount has not provided one yet.
	Address *CustomerAddress
	// TaxIDs is the desired set of tax registrations. The reconciler
	// reconciles this against the upstream Customer.tax_ids list.
	TaxIDs []TaxIDDetails
}

// CustomerAddress mirrors Stripe's address sub-object.
type CustomerAddress struct {
	Country    string
	Line1      string
	Line2      string
	City       string
	State      string
	PostalCode string
}

// TaxIDDetails is a single tax registration to ensure on the Customer.
type TaxIDDetails struct {
	Type  string // Stripe tax_id_data.type (e.g. "gb_vat").
	Value string
}

// EnsureCustomer creates or updates a Stripe Customer for the supplied
// billing account. When existingID is empty a new Customer is created;
// otherwise the existing one is updated in place. Returns the canonical
// `cus_…` identifier in both cases.
func (c *Client) EnsureCustomer(ctx context.Context, existingID, billingAccountName string, details CustomerDetails) (string, error) {
	if existingID == "" {
		params := &stripego.CustomerParams{
			Params: stripego.Params{
				Context: ctx,
				Metadata: map[string]string{
					"billing_account": billingAccountName,
				},
			},
		}
		applyCustomerDetails(params, details)
		cu, err := c.api.Customers.New(params)
		if err != nil {
			return "", fmt.Errorf("creating Stripe customer: %w", err)
		}
		// Tax IDs aren't settable on creation — apply them on a follow-up
		// reconcileTaxIDs call so the create path stays idempotent.
		if err := c.reconcileTaxIDs(ctx, cu.ID, details.TaxIDs); err != nil {
			return cu.ID, err
		}
		return cu.ID, nil
	}

	params := &stripego.CustomerParams{
		Params: stripego.Params{Context: ctx},
	}
	applyCustomerDetails(params, details)
	if _, err := c.api.Customers.Update(existingID, params); err != nil {
		return existingID, fmt.Errorf("updating Stripe customer %q: %w", existingID, err)
	}
	if err := c.reconcileTaxIDs(ctx, existingID, details.TaxIDs); err != nil {
		return existingID, err
	}
	return existingID, nil
}

func applyCustomerDetails(params *stripego.CustomerParams, d CustomerDetails) {
	if d.Name != "" {
		params.Name = stripego.String(d.Name)
	}
	if d.Email != "" {
		params.Email = stripego.String(d.Email)
	}
	if d.Address != nil {
		params.Address = &stripego.AddressParams{
			Country:    nilIfEmpty(d.Address.Country),
			Line1:      nilIfEmpty(d.Address.Line1),
			Line2:      nilIfEmpty(d.Address.Line2),
			City:       nilIfEmpty(d.Address.City),
			State:      nilIfEmpty(d.Address.State),
			PostalCode: nilIfEmpty(d.Address.PostalCode),
		}
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return stripego.String(s)
}

// reconcileTaxIDs ensures the Stripe Customer's tax_ids match the
// desired set. The Stripe API doesn't support a bulk-replace, so we
// list, diff, and apply Create/Delete deltas individually. Idempotent.
//
// The billing schema TaxID.type vocabulary is vendor-neutral
// snake-case (gb_vat, eu_vat, …). Today these match Stripe's
// tax_id_data.type values 1:1 so no translation is needed. If that
// ever diverges, map here — not in the billing CRD.
func (c *Client) reconcileTaxIDs(ctx context.Context, customerID string, desired []TaxIDDetails) error {
	if customerID == "" {
		return nil
	}
	// Build the existing set.
	existing := map[string]string{} // key = "type=value", val = stripe txi_… id
	iter := c.api.TaxIDs.List(&stripego.TaxIDListParams{
		Customer:   stripego.String(customerID),
		ListParams: stripego.ListParams{Context: ctx},
	})
	for iter.Next() {
		t := iter.TaxID()
		existing[string(t.Type)+"="+t.Value] = t.ID
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("listing Customer tax_ids on %q: %w", customerID, err)
	}

	desiredKeys := map[string]struct{}{}
	for _, d := range desired {
		key := d.Type + "=" + d.Value
		desiredKeys[key] = struct{}{}
		if _, ok := existing[key]; ok {
			continue
		}
		if _, err := c.api.TaxIDs.New(&stripego.TaxIDParams{
			Customer: stripego.String(customerID),
			Type:     stripego.String(d.Type),
			Value:    stripego.String(d.Value),
			Params:   stripego.Params{Context: ctx},
		}); err != nil {
			return fmt.Errorf("creating tax_id %s=%s on Customer %q: %w", d.Type, d.Value, customerID, err)
		}
	}
	for key, id := range existing {
		if _, want := desiredKeys[key]; want {
			continue
		}
		if _, err := c.api.TaxIDs.Del(id, &stripego.TaxIDParams{
			Customer: stripego.String(customerID),
			Params:   stripego.Params{Context: ctx},
		}); err != nil {
			return fmt.Errorf("deleting tax_id %q on Customer %q: %w", id, customerID, err)
		}
	}
	return nil
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
	ID             string
	Type           string
	Brand          string
	Last4          string
	BIN            string
	Country        string
	ExpMonth       int32
	ExpYear        int32
	AVSResult      string
	CVCResult      string
	BillingAddress *PaymentMethodBillingAddress
}

// PaymentMethodBillingAddress is the cardholder address as recorded
// against the confirmed payment method (Stripe billing_details.address).
type PaymentMethodBillingAddress struct {
	Country    string
	Line1      string
	Line2      string
	City       string
	State      string
	PostalCode string
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
	if pm.BillingDetails != nil && pm.BillingDetails.Address != nil {
		a := pm.BillingDetails.Address
		if a.Country != "" || a.Line1 != "" || a.Line2 != "" || a.City != "" || a.State != "" || a.PostalCode != "" {
			out.BillingAddress = &PaymentMethodBillingAddress{
				Country:    a.Country,
				Line1:      a.Line1,
				Line2:      a.Line2,
				City:       a.City,
				State:      a.State,
				PostalCode: a.PostalCode,
			}
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
