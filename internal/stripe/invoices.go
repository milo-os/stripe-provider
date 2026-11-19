// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	stripego "github.com/stripe/stripe-go/v87"
)

const (
	// MetadataLineKey is the metadata key holding a line's key, on the
	// Stripe invoice item and its invoice line.
	MetadataLineKey = "datum_line_key"
	// MetadataStripeInvoice names the StripeInvoice ("namespace/name") on
	// the Stripe invoice it made.
	MetadataStripeInvoice = "datum_stripe_invoice"
	// MetadataStripeInvoiceUID is the StripeInvoice's UID, so an invoice
	// made before a lost status write is adopted only by its own resource.
	MetadataStripeInvoiceUID = "datum_stripe_invoice_uid"
	// MetadataPurpose is the StripeInvoice's purpose. Preview drafts carry
	// "PreviewDraft", so nothing projects them as real invoices.
	MetadataPurpose = "datum_purpose"
	// MetadataNote holds a line's note, for staff. Metadata isn't printed
	// on invoices.
	MetadataNote = "datum_note"
)

// NontaxableTaxCode is Stripe's tax code for a product that's never taxed.
const NontaxableTaxCode = "txcd_00000000"

// InvoiceDraft is an invoice for Stripe to work out.
type InvoiceDraft struct {
	// CustomerID is the Stripe customer invoiced.
	CustomerID string
	// Currency is the ISO 4217 code, e.g. "USD".
	Currency string
	// Description is the invoice's memo.
	Description string
	// Metadata is stamped on the invoice.
	Metadata map[string]string
	// IdempotencyKey prefixes the idempotency keys of the writes, so a
	// retried create is the same create.
	IdempotencyKey string
	Lines          []InvoiceLine
}

// InvoiceLine is one invoice item.
type InvoiceLine struct {
	Key         string
	Description string
	// ProductID is the Stripe Product sold.
	ProductID string
	Quantity  decimal.Decimal
	// UnitAmount is the price of one, in the currency's major unit.
	UnitAmount decimal.Decimal
	// Start and End are the service period, both included.
	Start, End time.Time
	// TaxCode overrides the Product's tax code when set.
	TaxCode string
	// DiscountPercent is taken off the line when positive.
	DiscountPercent decimal.Decimal
	// Note is for staff, kept in the item's metadata.
	Note string
	// Metadata is stamped on the item, each key prefixed "datum_", with
	// the line's key.
	Metadata map[string]string
}

// InvoiceResult is Stripe's working of an invoice, in minor units.
type InvoiceResult struct {
	// ID is the invoice's ID: an "upcoming_in_" ID for a preview.
	ID       string
	Status   string
	Subtotal int64
	Discount int64
	Tax      int64
	Total    int64
	// StartingBalance is the customer's balance carried onto the invoice:
	// positive for what they owe, negative for credit.
	StartingBalance int64
	// AmountDue is Total with the starting balance applied.
	AmountDue int64
	Lines     []LineResult
}

// LineResult is Stripe's amounts for one line, in minor units.
type LineResult struct {
	Key      string
	Amount   int64
	Discount int64
	Tax      int64
}

// PreviewInvoice asks Stripe what the invoice would be, tax included,
// without creating anything.
func (c *Client) PreviewInvoice(ctx context.Context, d InvoiceDraft) (*InvoiceResult, error) {
	params := &stripego.InvoiceCreatePreviewParams{
		Customer:     stripego.String(d.CustomerID),
		Currency:     stripego.String(strings.ToLower(d.Currency)),
		AutomaticTax: &stripego.InvoiceCreatePreviewAutomaticTaxParams{Enabled: stripego.Bool(true)},
	}
	for _, l := range d.Lines {
		item := &stripego.InvoiceCreatePreviewInvoiceItemParams{
			Description: stripego.String(l.Description),
			PriceData: &stripego.InvoiceCreatePreviewInvoiceItemPriceDataParams{
				Currency:          stripego.String(strings.ToLower(d.Currency)),
				Product:           stripego.String(l.ProductID),
				TaxBehavior:       stripego.String("exclusive"),
				UnitAmountDecimal: ptrDecimal(minorUnits(l.UnitAmount)),
			},
			QuantityDecimal: ptrDecimal(l.Quantity),
			Period: &stripego.InvoiceCreatePreviewInvoiceItemPeriodParams{
				Start: stripego.Int64(l.Start.Unix()),
				End:   stripego.Int64(l.End.Unix()),
			},
			Metadata: lineMetadata(l),
		}
		if l.TaxCode != "" {
			item.TaxCode = stripego.String(l.TaxCode)
		}
		if l.DiscountPercent.IsPositive() {
			coupon, err := c.ensureCoupon(ctx, l.DiscountPercent)
			if err != nil {
				return nil, err
			}
			item.Discounts = []*stripego.InvoiceCreatePreviewInvoiceItemDiscountParams{{Coupon: stripego.String(coupon)}}
		}
		params.InvoiceItems = append(params.InvoiceItems, item)
	}
	inv, err := c.api.V1Invoices.CreatePreview(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("previewing Stripe invoice: %w", err)
	}
	return c.invoiceResult(ctx, inv)
}

// EnsureDraftInvoice makes the draft invoice d describes, or finishes the
// one an earlier attempt made: existingID when the caller recorded it, or
// a draft of the customer whose metadata names d. It's never finalized:
// auto_advance is off. Items are created on the draft itself, never left
// pending on the customer, where Stripe could add them to a real invoice.
func (c *Client) EnsureDraftInvoice(ctx context.Context, existingID string, d InvoiceDraft) (*InvoiceResult, error) {
	id := existingID
	if id == "" {
		found, err := c.findDraftInvoice(ctx, d)
		if err != nil {
			return nil, err
		}
		id = found
	}
	if id == "" {
		params := &stripego.InvoiceCreateParams{
			Customer:                    stripego.String(d.CustomerID),
			Currency:                    stripego.String(strings.ToLower(d.Currency)),
			AutoAdvance:                 stripego.Bool(false),
			CollectionMethod:            stripego.String("charge_automatically"),
			PendingInvoiceItemsBehavior: stripego.String("exclude"),
			AutomaticTax:                &stripego.InvoiceCreateAutomaticTaxParams{Enabled: stripego.Bool(true)},
			Description:                 stripego.String(d.Description),
			Metadata:                    d.Metadata,
		}
		params.SetIdempotencyKey(d.IdempotencyKey + "-invoice")
		inv, err := c.api.V1Invoices.Create(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("creating Stripe draft invoice: %w", err)
		}
		id = inv.ID
	}

	have, err := c.lineKeys(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, l := range d.Lines {
		if have[l.Key] {
			continue
		}
		item := &stripego.InvoiceItemCreateParams{
			Customer:    stripego.String(d.CustomerID),
			Invoice:     stripego.String(id),
			Description: stripego.String(l.Description),
			PriceData: &stripego.InvoiceItemCreatePriceDataParams{
				Currency:          stripego.String(strings.ToLower(d.Currency)),
				Product:           stripego.String(l.ProductID),
				TaxBehavior:       stripego.String("exclusive"),
				UnitAmountDecimal: ptrDecimal(minorUnits(l.UnitAmount)),
			},
			QuantityDecimal: ptrDecimal(l.Quantity),
			Period: &stripego.InvoiceItemCreatePeriodParams{
				Start: stripego.Int64(l.Start.Unix()),
				End:   stripego.Int64(l.End.Unix()),
			},
			Metadata: lineMetadata(l),
		}
		if l.TaxCode != "" {
			item.TaxCode = stripego.String(l.TaxCode)
		}
		if l.DiscountPercent.IsPositive() {
			coupon, err := c.ensureCoupon(ctx, l.DiscountPercent)
			if err != nil {
				return nil, err
			}
			item.Discounts = []*stripego.InvoiceItemCreateDiscountParams{{Coupon: stripego.String(coupon)}}
		}
		item.SetIdempotencyKey(d.IdempotencyKey + "-line-" + l.Key)
		if _, err := c.api.V1InvoiceItems.Create(ctx, item); err != nil {
			return &InvoiceResult{ID: id}, fmt.Errorf("adding line %q to Stripe draft %s: %w", l.Key, id, err)
		}
	}

	inv, err := c.api.V1Invoices.Retrieve(ctx, id, &stripego.InvoiceRetrieveParams{})
	if err != nil {
		return &InvoiceResult{ID: id}, fmt.Errorf("retrieving Stripe draft %s: %w", id, err)
	}
	return c.invoiceResult(ctx, inv)
}

// ErrInvoiceNotDraft is returned by DeleteDraftInvoice for an invoice
// someone finalized, which Stripe won't delete.
var ErrInvoiceNotDraft = errors.New("the Stripe invoice is no longer a draft")

// DeleteDraftInvoice deletes a draft invoice. One already gone is fine.
func (c *Client) DeleteDraftInvoice(ctx context.Context, id string) error {
	inv, err := c.api.V1Invoices.Retrieve(ctx, id, &stripego.InvoiceRetrieveParams{})
	if isResourceMissing(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("retrieving Stripe invoice %s: %w", id, err)
	}
	if inv.Status != stripego.InvoiceStatusDraft {
		return fmt.Errorf("%w: %s is %s", ErrInvoiceNotDraft, id, inv.Status)
	}
	if _, err := c.api.V1Invoices.Delete(ctx, id, &stripego.InvoiceDeleteParams{}); err != nil && !isResourceMissing(err) {
		return fmt.Errorf("deleting Stripe draft %s: %w", id, err)
	}
	return nil
}

// DashboardInvoiceURL is the Stripe dashboard's page for an invoice, in
// test mode when the key is a test key.
func (c *Client) DashboardInvoiceURL(id string) string {
	base := "https://dashboard.stripe.com/"
	if c.testMode {
		base += "test/"
	}
	return base + "invoices/" + id
}

// findDraftInvoice returns the customer's draft whose metadata names d's
// StripeInvoice, or "".
func (c *Client) findDraftInvoice(ctx context.Context, d InvoiceDraft) (string, error) {
	uid := d.Metadata[MetadataStripeInvoiceUID]
	if uid == "" {
		return "", nil
	}
	params := &stripego.InvoiceListParams{
		Customer: stripego.String(d.CustomerID),
		Status:   stripego.String(string(stripego.InvoiceStatusDraft)),
	}
	for inv, err := range c.api.V1Invoices.List(ctx, params).All(ctx) {
		if err != nil {
			return "", fmt.Errorf("listing Stripe drafts: %w", err)
		}
		if inv.Metadata[MetadataStripeInvoiceUID] == uid {
			return inv.ID, nil
		}
	}
	return "", nil
}

// lineKeys are the keys of the lines already on an invoice.
func (c *Client) lineKeys(ctx context.Context, id string) (map[string]bool, error) {
	keys := map[string]bool{}
	for line, err := range c.api.V1Invoices.ListLines(ctx, &stripego.InvoiceListLinesParams{Invoice: stripego.String(id)}).All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("listing lines of Stripe draft %s: %w", id, err)
		}
		if k := line.Metadata[MetadataLineKey]; k != "" {
			keys[k] = true
		}
	}
	return keys, nil
}

// invoiceResult reads Stripe's amounts. A preview holds only its first
// lines inline, so the rest are listed.
func (c *Client) invoiceResult(ctx context.Context, inv *stripego.Invoice) (*InvoiceResult, error) {
	res := &InvoiceResult{
		ID: inv.ID, Status: string(inv.Status), Subtotal: inv.Subtotal, Total: inv.Total,
		StartingBalance: inv.StartingBalance, AmountDue: inv.AmountDue,
	}
	for _, t := range inv.TotalTaxes {
		res.Tax += t.Amount
	}
	for _, d := range inv.TotalDiscountAmounts {
		res.Discount += d.Amount
	}
	var lines []*stripego.InvoiceLineItem
	if inv.Lines != nil {
		lines = inv.Lines.Data
	}
	if inv.Lines != nil && inv.Lines.HasMore && inv.ID != "" {
		lines = nil
		for line, err := range c.api.V1Invoices.ListLines(ctx, &stripego.InvoiceListLinesParams{Invoice: stripego.String(inv.ID)}).All(ctx) {
			if err != nil {
				return nil, fmt.Errorf("listing lines of Stripe invoice %s: %w", inv.ID, err)
			}
			lines = append(lines, line)
		}
	}
	for _, line := range lines {
		lr := LineResult{Key: line.Metadata[MetadataLineKey], Amount: line.Amount}
		for _, d := range line.DiscountAmounts {
			lr.Discount += d.Amount
		}
		for _, t := range line.Taxes {
			lr.Tax += t.Amount
		}
		res.Lines = append(res.Lines, lr)
	}
	return res, nil
}

// ensureCoupon returns the standing coupon for a percentage off, creating
// it on first use. Its ID is fixed, so it's never made twice.
func (c *Client) ensureCoupon(ctx context.Context, percent decimal.Decimal) (string, error) {
	id := "datum-pct-" + strings.ReplaceAll(percent.String(), ".", "_")
	if _, err := c.api.V1Coupons.Retrieve(ctx, id, &stripego.CouponRetrieveParams{}); err == nil {
		return id, nil
	} else if !isResourceMissing(err) {
		return "", fmt.Errorf("retrieving Stripe coupon %s: %w", id, err)
	}
	off, _ := percent.Float64()
	_, err := c.api.V1Coupons.Create(ctx, &stripego.CouponCreateParams{
		ID:         stripego.String(id),
		Name:       stripego.String(percent.String() + "% off"),
		PercentOff: stripego.Float64(off),
		Duration:   stripego.String("forever"),
	})
	var stripeErr *stripego.Error
	if err != nil && (!errors.As(err, &stripeErr) || stripeErr.Code != stripego.ErrorCodeResourceAlreadyExists) {
		// Another reconcile creating the same coupon isn't a failure.
		return "", fmt.Errorf("creating Stripe coupon %s: %w", id, err)
	}
	return id, nil
}

func lineMetadata(l InvoiceLine) map[string]string {
	md := map[string]string{MetadataLineKey: l.Key}
	for k, v := range l.Metadata {
		md["datum_"+k] = v
	}
	if l.Note != "" {
		md[MetadataNote] = l.Note
	}
	return md
}

// minorUnits converts an amount in the currency's major unit to cents.
func minorUnits(amount decimal.Decimal) decimal.Decimal {
	return amount.Shift(2)
}

func ptrDecimal(d decimal.Decimal) *decimal.Decimal { return &d }
