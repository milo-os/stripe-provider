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
	// BusinessName is the legal entity that pays, when set on the
	// BillingAccount. Stripe's standard Customer object has a single
	// `name` field that Name above already covers (BusinessName takes
	// precedence in the mapper). This field is surfaced separately so
	// the reconciler can stamp both the business and the individual
	// name on Customer.metadata — the Stripe dashboard's "Business
	// name" / "Individual name" detail rows are *not* derived from
	// Customer.name and have no equivalent in the standard Customer
	// API at v81 of stripe-go. Metadata is the next-best surface:
	// queryable, indexable in Stripe Sigma, and visible in the
	// metadata pane of the dashboard. Empty string clears the key.
	BusinessName string
	// IndividualName is the human billing contact, when set on the
	// BillingAccount. See BusinessName for the metadata-surfacing
	// rationale. Empty string clears the key.
	IndividualName string
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
// billing account. When existingID is empty the caller is asking us to
// either find or create the matching Customer. Before creating we ask
// Stripe whether a Customer tagged with `metadata.billing_account =
// <name>` already exists, so a controller restart that lost local
// status (or a freshly-rebuilt cluster) doesn't mint duplicate
// Customers. Returns the canonical `cus_…` identifier in all cases.
func (c *Client) EnsureCustomer(ctx context.Context, existingID, billingAccountName string, details CustomerDetails) (string, error) {
	if existingID == "" && billingAccountName != "" {
		found, err := c.findCustomerByBillingAccount(ctx, billingAccountName)
		if err != nil {
			return "", fmt.Errorf("searching Stripe customer for BillingAccount %q: %w", billingAccountName, err)
		}
		existingID = found
	}

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
			return cu.ID, &TaxIDError{Underlying: err}
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
		return existingID, &TaxIDError{Underlying: err}
	}
	return existingID, nil
}

// findCustomerByBillingAccount queries Stripe's customer search for the
// first Customer carrying `metadata.billing_account = name`. Returns
// "" when no match exists. Errors are surfaced to the caller so the
// reconciler can requeue rather than silently fall back to creating a
// duplicate; transient search outages are loud rather than corrupting
// state.
//
// Stripe customer search is eventually consistent (typically <1s after a
// create), so a tight create/lookup cycle on a fresh BillingAccount may
// still produce a duplicate. The local-state lookup
// (findExistingCustomerID in the controller package) catches the common
// case; this exists for cold-start recovery scenarios where local
// status was lost.
func (c *Client) findCustomerByBillingAccount(ctx context.Context, billingAccountName string) (string, error) {
	params := &stripego.CustomerSearchParams{
		SearchParams: stripego.SearchParams{
			Context: ctx,
			Query:   fmt.Sprintf("metadata['billing_account']:'%s'", escapeSearchQueryValue(billingAccountName)),
			Limit:   stripego.Int64(1),
		},
	}
	iter := c.api.Customers.Search(params)
	if iter.Next() {
		return iter.Customer().ID, nil
	}
	if err := iter.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// escapeSearchQueryValue escapes single quotes in a Stripe search query
// value. BillingAccount names are generated slugs today so this is
// belt-and-braces, but the surface area is small enough to bake in.
func escapeSearchQueryValue(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\'' || v[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, v[i])
	}
	return string(out)
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
	// Always stamp business / individual name on metadata, even when
	// empty — sending an empty value clears the key per Stripe's
	// metadata merge semantics, which is the right behaviour when a
	// user removes the value from the BillingAccount. Preserve any
	// keys the caller set before us (Customer.New uses metadata to
	// record `billing_account` for dedup).
	if params.Params.Metadata == nil {
		params.Params.Metadata = map[string]string{}
	}
	params.Params.Metadata["business_name"] = d.BusinessName
	params.Params.Metadata["individual_name"] = d.IndividualName
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
	ID   string
	Type string
	// Name is the cardholder name from PaymentMethod.billing_details.name,
	// i.e. whatever the user typed into the Stripe Elements "Full name"
	// input on the add-card form. Surfaced so the webhook can backfill
	// the Customer-level name when the BillingAccount didn't carry one.
	Name           string
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
	if pm.BillingDetails != nil {
		out.Name = pm.BillingDetails.Name
		if pm.BillingDetails.Address != nil {
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
	}
	return out, nil
}

// BackfillCustomerName sets Stripe Customer.name to the supplied value
// only when the upstream record carries no name yet. The webhook uses
// this after a SetupIntent confirms to lift the cardholder name from
// PaymentMethod.billing_details.name onto the Customer (so the Stripe
// dashboard's "Individual name" field is populated) without
// overwriting a business or contact name a user has already entered
// via the BillingAccount form.
//
// Idempotent: empty inputs, an already-set Customer.name, or a missing
// Customer all return nil.
func (c *Client) BackfillCustomerName(ctx context.Context, customerID, name string) error {
	if customerID == "" || name == "" {
		return nil
	}
	cu, err := c.api.Customers.Get(customerID, &stripego.CustomerParams{
		Params: stripego.Params{Context: ctx},
	})
	if err != nil {
		if stripeErr, ok := err.(*stripego.Error); ok && stripeErr.Code == stripego.ErrorCodeResourceMissing {
			return nil
		}
		return fmt.Errorf("getting Stripe customer %q: %w", customerID, err)
	}
	if cu.Name != "" {
		return nil
	}
	if _, err := c.api.Customers.Update(customerID, &stripego.CustomerParams{
		Name:   stripego.String(name),
		Params: stripego.Params{Context: ctx},
	}); err != nil {
		return fmt.Errorf("backfilling Stripe customer %q name: %w", customerID, err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
