// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"errors"
	"fmt"

	stripego "github.com/stripe/stripe-go/v87"
)

// ProductDetails is the Stripe Product a StripeProduct keeps in step.
type ProductDetails struct {
	// ID is the Product's ID. The caller chooses it, so a create that's
	// retried after a lost response finds the Product it made rather than
	// making another.
	ID string
	// Name is the Product's name.
	Name string
	// TaxCode is the Product's Stripe tax code, or "" for the account's
	// default.
	TaxCode string
	// Active is false for a Product nothing sells any more.
	Active bool
	// Metadata is stamped on the Product. Keys already on the Product and
	// not here are left alone.
	Metadata map[string]string
}

// EnsureProduct creates the Product, or updates it to match d when it
// already exists. Only fields that differ are sent.
func (c *Client) EnsureProduct(ctx context.Context, d ProductDetails) error {
	if d.ID == "" || d.Name == "" {
		return errors.New("EnsureProduct requires an id and a name")
	}
	existing, err := c.api.V1Products.Retrieve(ctx, d.ID, &stripego.ProductRetrieveParams{})
	if isResourceMissing(err) {
		return c.createProduct(ctx, d)
	}
	if err != nil {
		return fmt.Errorf("retrieving Stripe product %q: %w", d.ID, err)
	}
	return c.updateProduct(ctx, existing, d)
}

func (c *Client) createProduct(ctx context.Context, d ProductDetails) error {
	params := &stripego.ProductCreateParams{
		ID:       stripego.String(d.ID),
		Name:     stripego.String(d.Name),
		Active:   stripego.Bool(d.Active),
		Metadata: d.Metadata,
	}
	if d.TaxCode != "" {
		params.TaxCode = stripego.String(d.TaxCode)
	}
	_, err := c.api.V1Products.Create(ctx, params)
	var stripeErr *stripego.Error
	if errors.As(err, &stripeErr) && stripeErr.Code == stripego.ErrorCodeResourceAlreadyExists {
		// Made by an earlier attempt whose response was lost: bring it in
		// line instead.
		existing, getErr := c.api.V1Products.Retrieve(ctx, d.ID, &stripego.ProductRetrieveParams{})
		if getErr != nil {
			return fmt.Errorf("retrieving Stripe product %q: %w", d.ID, getErr)
		}
		return c.updateProduct(ctx, existing, d)
	}
	if err != nil {
		return fmt.Errorf("creating Stripe product %q: %w", d.ID, err)
	}
	return nil
}

func (c *Client) updateProduct(ctx context.Context, existing *stripego.Product, d ProductDetails) error {
	params := &stripego.ProductUpdateParams{}
	changed := false
	if existing.Name != d.Name {
		params.Name = stripego.String(d.Name)
		changed = true
	}
	if existing.Active != d.Active {
		params.Active = stripego.Bool(d.Active)
		changed = true
	}
	switch current := productTaxCode(existing); {
	case d.TaxCode == "" && current != "":
		params.UnsetFields = append(params.UnsetFields, stripego.ProductUpdateParamsUnsetFieldTaxCode)
		changed = true
	case d.TaxCode != "" && d.TaxCode != current:
		params.TaxCode = stripego.String(d.TaxCode)
		changed = true
	}
	md := map[string]string{}
	for k, v := range d.Metadata {
		if existing.Metadata[k] != v {
			md[k] = v
		}
	}
	if len(md) > 0 {
		params.Metadata = md
		changed = true
	}
	if !changed {
		return nil
	}
	if _, err := c.api.V1Products.Update(ctx, d.ID, params); err != nil {
		return fmt.Errorf("updating Stripe product %q: %w", d.ID, err)
	}
	return nil
}

// productTaxCode is the ID of a Product's tax code, or "".
func productTaxCode(p *stripego.Product) string {
	if p.TaxCode == nil {
		return ""
	}
	return p.TaxCode.ID
}

func isResourceMissing(err error) bool {
	var stripeErr *stripego.Error
	return errors.As(err, &stripeErr) && stripeErr.Code == stripego.ErrorCodeResourceMissing
}

// ProductMetadata is the metadata stripe-provider stamps on every Product
// it manages, naming the StripeProduct and, when it has one, the SKU.
func ProductMetadata(namespace, name, sku string) map[string]string {
	md := map[string]string{
		"datum_stripe_product": namespace + "/" + name,
		"datum_managed_by":     "stripe-provider",
	}
	if sku != "" {
		md["datum_sku"] = sku
	}
	return md
}
