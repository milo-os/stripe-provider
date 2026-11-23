// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
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
	// MetadataOpenMeterInvoiceID is OpenMeter's ID for an invoice it
	// assembled, the key OpenMeter's own Stripe integration used, so
	// openmeter-provider matches the Stripe invoice to OpenMeter's.
	MetadataOpenMeterInvoiceID = "om_invoice_id"
	// MetadataLineHash is a hash of everything a line is written from, on
	// its invoice item, so a line whose spec changed is rewritten.
	MetadataLineHash = "datum_line_hash"
)

// Collection methods for an invoice.
const (
	// CollectionChargeAutomatically charges the customer's default payment
	// method when the invoice is finalized.
	CollectionChargeAutomatically = "charge_automatically"
	// CollectionSendInvoice emails the invoice, due DaysUntilDue days later.
	CollectionSendInvoice = "send_invoice"
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
	// Revision is the source's version of the lines, such as its
	// generation. It's in each line's idempotency key, so a line removed
	// and later added back, unchanged, is written again rather than
	// replayed by Stripe as the item it already deleted.
	Revision string
	// CollectionMethod is CollectionChargeAutomatically (the default) or
	// CollectionSendInvoice.
	CollectionMethod string
	// DaysUntilDue is how long a sent invoice gives the customer to pay.
	DaysUntilDue int64
	Lines        []InvoiceLine
}

func (d InvoiceDraft) collectionMethod() string {
	if d.CollectionMethod == "" {
		return CollectionChargeAutomatically
	}
	return d.CollectionMethod
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
	// TaxCode is the line's tax code, when it has its own.
	TaxCode string
	// ProductTaxCode is the Product's tax code, "" for the account's
	// default. A line whose TaxCode is the same is taxed as its Product.
	ProductTaxCode string
	// DiscountPercent is taken off the line when positive.
	DiscountPercent decimal.Decimal
	// Note is for staff, kept in the item's metadata.
	Note string
	// Metadata is stamped on the item, each key prefixed "datum_", with
	// the line's key.
	Metadata map[string]string
}

// taxOverride is the tax code the line is taxed by in place of its
// Product's, or "" when it's taxed as its Product is.
//
// Stripe takes a tax code on an invoice item only without price_data, and
// price_data is what names the item's Product. So a line taxed as its
// Product (nearly every line: its pricing gives both) is written with its
// Product, and a line with a code of its own is written without one, its
// Product kept in its metadata as datum_product.
func (l InvoiceLine) taxOverride() string {
	if l.TaxCode == "" || l.TaxCode == l.ProductTaxCode {
		return ""
	}
	return l.TaxCode
}

// ErrorMessage is err's text with any Stripe error in it cut down to
// Stripe's own message. The full error carries a request ID and log link
// that differ on every attempt, so a status condition holding it would
// change, and requeue its object at once, each time the same call failed.
func ErrorMessage(err error) string {
	msg := err.Error()
	var se *stripego.Error
	if errors.As(err, &se) && se.Msg != "" {
		msg = strings.Replace(msg, se.Error(), se.Msg, 1)
	}
	return msg
}

// InvoiceResult is Stripe's working of an invoice, in minor units.
type InvoiceResult struct {
	// ID is the invoice's ID: an "upcoming_in_" ID for a preview.
	ID     string
	Status string
	// Number is given when the invoice is finalized.
	Number string
	// HostedInvoiceURL is the customer's page for a finalized invoice.
	HostedInvoiceURL string
	// FinalizedAt, PaidAt and DueDate are zero until they apply.
	FinalizedAt, PaidAt, DueDate time.Time
	AmountPaid, AmountRemaining  int64
	// AttemptCount is how many times Stripe has tried to charge it;
	// NextPaymentAttempt is when it tries again, zero when it won't.
	AttemptCount       int64
	NextPaymentAttempt time.Time
	Subtotal           int64
	Discount           int64
	Tax                int64
	Total              int64
	// StartingBalance is the customer's balance carried onto the invoice:
	// positive for what they owe, negative for credit.
	StartingBalance int64
	// AmountDue is Total with the starting balance applied.
	AmountDue int64
	Lines     []LineResult
}

// LineResult is Stripe's amounts for one line, in minor units.
type LineResult struct {
	Key string
	// InvoiceItemID is the invoice item behind the line.
	InvoiceItemID string
	Amount        int64
	Discount      int64
	Tax           int64
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
			Description:     stripego.String(l.Description),
			QuantityDecimal: ptrDecimal(l.Quantity),
			Period: &stripego.InvoiceCreatePreviewInvoiceItemPeriodParams{
				Start: stripego.Int64(l.Start.Unix()),
				End:   stripego.Int64(l.End.Unix()),
			},
			Metadata: lineMetadata(l),
		}
		if code := l.taxOverride(); code != "" {
			item.UnitAmountDecimal = ptrDecimal(minorUnits(l.UnitAmount))
			item.TaxBehavior = stripego.String("exclusive")
			item.TaxCode = stripego.String(code)
		} else {
			item.PriceData = &stripego.InvoiceCreatePreviewInvoiceItemPriceDataParams{
				Currency:          stripego.String(strings.ToLower(d.Currency)),
				Product:           stripego.String(l.ProductID),
				TaxBehavior:       stripego.String("exclusive"),
				UnitAmountDecimal: ptrDecimal(minorUnits(l.UnitAmount)),
			}
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
// a draft of the customer whose metadata names d. It's never finalized
// here: auto_advance is off. Items are created on the draft itself, never
// left pending on the customer, where Stripe could add them to another
// invoice.
//
// The draft's lines are made to match d's exactly: a line d no longer has
// is removed, one whose content changed is written again, and a missing
// one is added. Lines are matched by their key and content hash, kept on
// each invoice item's metadata.
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
			CollectionMethod:            stripego.String(d.collectionMethod()),
			PendingInvoiceItemsBehavior: stripego.String("exclude"),
			AutomaticTax:                &stripego.InvoiceCreateAutomaticTaxParams{Enabled: stripego.Bool(true)},
			Metadata:                    d.Metadata,
		}
		if d.Description != "" {
			params.Description = stripego.String(d.Description)
		}
		if d.collectionMethod() == CollectionSendInvoice {
			params.DaysUntilDue = stripego.Int64(d.DaysUntilDue)
		}
		params.SetIdempotencyKey(d.IdempotencyKey + "-invoice")
		inv, err := c.api.V1Invoices.Create(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("creating Stripe draft invoice: %w", err)
		}
		id = inv.ID
	}

	inv, err := c.api.V1Invoices.Retrieve(ctx, id, &stripego.InvoiceRetrieveParams{})
	if isResourceMissing(err) {
		return &InvoiceResult{ID: id}, fmt.Errorf("%w: %s", ErrInvoiceMissing, id)
	}
	if err != nil {
		return &InvoiceResult{ID: id}, fmt.Errorf("retrieving Stripe draft %s: %w", id, err)
	}
	if inv.Status != stripego.InvoiceStatusDraft {
		return c.invoiceResult(ctx, inv)
	}
	if err := c.syncCollection(ctx, inv, d); err != nil {
		return &InvoiceResult{ID: id}, err
	}
	if err := c.syncLines(ctx, id, d); err != nil {
		return &InvoiceResult{ID: id}, err
	}

	inv, err = c.api.V1Invoices.Retrieve(ctx, id, &stripego.InvoiceRetrieveParams{})
	if err != nil {
		return &InvoiceResult{ID: id}, fmt.Errorf("retrieving Stripe draft %s: %w", id, err)
	}
	return c.invoiceResult(ctx, inv)
}

// syncCollection sets the draft's collection method and terms to d's,
// which follow the account's payment arrangement, if they've changed.
func (c *Client) syncCollection(ctx context.Context, inv *stripego.Invoice, d InvoiceDraft) error {
	want := d.collectionMethod()
	if string(inv.CollectionMethod) == want &&
		(want != CollectionSendInvoice || inv.DueDate == 0 || daysBetween(inv.Created, inv.DueDate) == d.DaysUntilDue) {
		return nil
	}
	params := &stripego.InvoiceUpdateParams{CollectionMethod: stripego.String(want)}
	if want == CollectionSendInvoice {
		params.DaysUntilDue = stripego.Int64(d.DaysUntilDue)
	}
	if _, err := c.api.V1Invoices.Update(ctx, inv.ID, params); err != nil {
		return fmt.Errorf("setting collection on Stripe draft %s: %w", inv.ID, err)
	}
	return nil
}

func daysBetween(from, to int64) int64 {
	return (to - from + 12*3600) / (24 * 3600)
}

// syncLines makes the draft's invoice items match d's lines.
func (c *Client) syncLines(ctx context.Context, id string, d InvoiceDraft) error {
	want := map[string]string{}
	for _, l := range d.Lines {
		want[l.Key] = l.hash()
	}
	have := map[string]bool{}
	for line, err := range c.api.V1Invoices.ListLines(ctx, &stripego.InvoiceListLinesParams{Invoice: stripego.String(id)}).All(ctx) {
		if err != nil {
			return fmt.Errorf("listing lines of Stripe draft %s: %w", id, err)
		}
		key, hash := line.Metadata[MetadataLineKey], line.Metadata[MetadataLineHash]
		// A preview draft written before lines carried a hash keeps them.
		current := want[key] != "" && (hash == want[key] || (hash == "" && d.Metadata[MetadataPurpose] == "PreviewDraft"))
		if current && !have[key] {
			have[key] = true
			continue
		}
		item := lineInvoiceItem(line)
		if item == "" {
			return fmt.Errorf("line %s of Stripe draft %s has no invoice item to remove", line.ID, id)
		}
		if _, err := c.api.V1InvoiceItems.Delete(ctx, item, &stripego.InvoiceItemDeleteParams{}); err != nil && !isResourceMissing(err) {
			return fmt.Errorf("removing line %q from Stripe draft %s: %w", key, id, err)
		}
	}
	for _, l := range d.Lines {
		if have[l.Key] {
			continue
		}
		item := &stripego.InvoiceItemCreateParams{
			Customer:        stripego.String(d.CustomerID),
			Invoice:         stripego.String(id),
			Description:     stripego.String(l.Description),
			QuantityDecimal: ptrDecimal(l.Quantity),
			Period: &stripego.InvoiceItemCreatePeriodParams{
				Start: stripego.Int64(l.Start.Unix()),
				End:   stripego.Int64(l.End.Unix()),
			},
			Metadata: lineMetadata(l),
		}
		if code := l.taxOverride(); code != "" {
			item.UnitAmountDecimal = ptrDecimal(minorUnits(l.UnitAmount))
			item.TaxBehavior = stripego.String("exclusive")
			item.TaxCode = stripego.String(code)
		} else {
			item.PriceData = &stripego.InvoiceItemCreatePriceDataParams{
				Currency:          stripego.String(strings.ToLower(d.Currency)),
				Product:           stripego.String(l.ProductID),
				TaxBehavior:       stripego.String("exclusive"),
				UnitAmountDecimal: ptrDecimal(minorUnits(l.UnitAmount)),
			}
		}
		if l.DiscountPercent.IsPositive() {
			coupon, err := c.ensureCoupon(ctx, l.DiscountPercent)
			if err != nil {
				return err
			}
			item.Discounts = []*stripego.InvoiceItemCreateDiscountParams{{Coupon: stripego.String(coupon)}}
		}
		// The revision and hash are in the key, so a line written again,
		// after a change or after being removed and added back, isn't
		// mistaken by Stripe for a retry of an earlier write.
		item.SetIdempotencyKey(d.IdempotencyKey + "-line-" + l.Key + "-r" + d.Revision + "-" + l.hash())
		if _, err := c.api.V1InvoiceItems.Create(ctx, item); err != nil {
			return fmt.Errorf("adding line %q to Stripe draft %s: %w", l.Key, id, err)
		}
	}
	return nil
}

func lineInvoiceItem(line *stripego.InvoiceLineItem) string {
	if line.Parent != nil && line.Parent.InvoiceItemDetails != nil {
		return line.Parent.InvoiceItemDetails.InvoiceItem
	}
	return ""
}

// FinalizeInvoice finalizes a draft so Stripe charges or sends it, by its
// collection method. An invoice that's already past draft is just read.
func (c *Client) FinalizeInvoice(ctx context.Context, id, idempotencyKey string) (*InvoiceResult, error) {
	inv, err := c.api.V1Invoices.Retrieve(ctx, id, &stripego.InvoiceRetrieveParams{})
	if err != nil {
		return nil, fmt.Errorf("retrieving Stripe invoice %s: %w", id, err)
	}
	if inv.Status == stripego.InvoiceStatusDraft {
		// A draft keeps the tax Stripe worked out when it last changed: one
		// written while its customer had no address stays without a tax
		// location, and Stripe refuses to finalize it
		// (customer_tax_location_invalid) even once the customer has one.
		// Updating the draft has Stripe work its tax out again.
		if _, err := c.api.V1Invoices.Update(ctx, id, &stripego.InvoiceUpdateParams{
			AutomaticTax: &stripego.InvoiceUpdateAutomaticTaxParams{Enabled: stripego.Bool(true)},
		}); err != nil {
			return nil, fmt.Errorf("recalculating tax on Stripe invoice %s: %w", id, err)
		}
		params := &stripego.InvoiceFinalizeInvoiceParams{AutoAdvance: stripego.Bool(true)}
		params.SetIdempotencyKey(idempotencyKey + "-finalize")
		inv, err = c.api.V1Invoices.FinalizeInvoice(ctx, id, params)
		if err != nil {
			return nil, fmt.Errorf("finalizing Stripe invoice %s: %w", id, err)
		}
	}
	return c.invoiceResult(ctx, inv)
}

// RetrieveInvoice reads an invoice's state and amounts.
func (c *Client) RetrieveInvoice(ctx context.Context, id string) (*InvoiceResult, error) {
	inv, err := c.api.V1Invoices.Retrieve(ctx, id, &stripego.InvoiceRetrieveParams{})
	if isResourceMissing(err) {
		return nil, fmt.Errorf("%w: %s", ErrInvoiceMissing, id)
	}
	if err != nil {
		return nil, fmt.Errorf("retrieving Stripe invoice %s: %w", id, err)
	}
	return c.invoiceResult(ctx, inv)
}

// ErrInvoiceMissing is returned by RetrieveInvoice for an invoice Stripe
// doesn't have, such as a draft someone deleted.
var ErrInvoiceMissing = errors.New("the Stripe invoice doesn't exist")

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

// invoiceResult reads Stripe's amounts. A preview holds only its first
// lines inline, so the rest are listed.
func (c *Client) invoiceResult(ctx context.Context, inv *stripego.Invoice) (*InvoiceResult, error) {
	res := &InvoiceResult{
		ID: inv.ID, Status: string(inv.Status), Subtotal: inv.Subtotal, Total: inv.Total,
		StartingBalance: inv.StartingBalance, AmountDue: inv.AmountDue,
		Number: inv.Number, HostedInvoiceURL: inv.HostedInvoiceURL,
		AmountPaid: inv.AmountPaid, AmountRemaining: inv.AmountRemaining,
		DueDate: unixTime(inv.DueDate), AttemptCount: inv.AttemptCount,
		NextPaymentAttempt: unixTime(inv.NextPaymentAttempt),
	}
	if t := inv.StatusTransitions; t != nil {
		res.FinalizedAt, res.PaidAt = unixTime(t.FinalizedAt), unixTime(t.PaidAt)
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
		lr := LineResult{Key: line.Metadata[MetadataLineKey], InvoiceItemID: lineInvoiceItem(line), Amount: line.Amount}
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
	md := map[string]string{MetadataLineKey: l.Key, MetadataLineHash: l.hash()}
	if l.taxOverride() != "" {
		// Written without price_data, so without its Product: kept here.
		md["datum_product"] = l.ProductID
	}
	for k, v := range l.Metadata {
		md["datum_"+k] = v
	}
	if l.Note != "" {
		md[MetadataNote] = l.Note
	}
	return md
}

// hash is a short hash of everything the line's invoice item is written
// from.
func (l InvoiceLine) hash() string {
	keys := make([]string, 0, len(l.Metadata))
	for k := range l.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s\x00%s\x00%s",
		l.Key, l.Description, l.ProductID, l.Quantity.String(), l.UnitAmount.String(),
		l.Start.Unix(), l.End.Unix(), l.TaxCode, l.DiscountPercent.String(), l.Note)
	for _, k := range keys {
		fmt.Fprintf(&b, "\x00%s=%s", k, l.Metadata[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// minorUnits converts an amount in the currency's major unit to cents.
func minorUnits(amount decimal.Decimal) decimal.Decimal {
	return amount.Shift(2)
}

func ptrDecimal(d decimal.Decimal) *decimal.Decimal { return &d }
