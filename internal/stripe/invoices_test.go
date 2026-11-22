// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// fakeInvoices serves the Stripe invoice, invoice item and coupon calls the
// client makes, recording each request's form.
type fakeInvoices struct {
	mu       sync.Mutex
	requests []fakeRequest
	// drafts are the customer's drafts listed by GET /v1/invoices.
	drafts string
	// lines are the draft's lines listed by GET /v1/invoices/in_1/lines.
	lines string
	// invoice is the draft returned by GET /v1/invoices/in_1.
	invoice string
	// coupons are the coupon IDs that exist.
	coupons map[string]bool
}

type fakeRequest struct {
	method, path string
	form         url.Values
	idempotency  string
}

const previewResponse = `{"id":"upcoming_in_1","object":"invoice","status":"draft","subtotal":63000,"total":61515,
  "starting_balance":23,"amount_due":61538,"total_taxes":[{"amount":3465}],"total_discount_amounts":[{"amount":4950}],
  "lines":{"object":"list","has_more":false,"data":[
    {"id":"il_1","object":"line_item","amount":49500,"metadata":{"datum_line_key":"rack"},
     "discount_amounts":[{"amount":4950}],"taxes":[{"amount":3465}]},
    {"id":"il_2","object":"line_item","amount":13500,"metadata":{"datum_line_key":"setup"},"taxes":[]}]}}`

func (f *fakeInvoices) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	if r.Method == http.MethodGet {
		form = r.URL.Query()
	}
	f.requests = append(f.requests, fakeRequest{r.Method, r.URL.Path, form, r.Header.Get("Idempotency-Key")})
	w.Header().Set("Content-Type", "application/json")
	missing := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_missing","type":"invalid_request_error"}}`))
	}
	switch {
	case r.URL.Path == "/v1/invoices/create_preview":
		_, _ = w.Write([]byte(previewResponse))
	case strings.HasPrefix(r.URL.Path, "/v1/coupons/") && r.Method == http.MethodGet:
		if f.coupons[strings.TrimPrefix(r.URL.Path, "/v1/coupons/")] {
			_, _ = w.Write([]byte(`{"id":"x","object":"coupon"}`))
			return
		}
		missing()
	case r.URL.Path == "/v1/coupons":
		_, _ = w.Write([]byte(`{"id":"x","object":"coupon"}`))
	case r.URL.Path == "/v1/invoices" && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(f.drafts))
	case r.URL.Path == "/v1/invoices" && r.Method == http.MethodPost:
		_, _ = w.Write([]byte(`{"id":"in_1","object":"invoice","status":"draft"}`))
	case r.URL.Path == "/v1/invoices/in_1/lines":
		_, _ = w.Write([]byte(f.lines))
	case r.URL.Path == "/v1/invoiceitems":
		_, _ = w.Write([]byte(`{"id":"ii_1","object":"invoiceitem"}`))
	case strings.HasPrefix(r.URL.Path, "/v1/invoiceitems/") && r.Method == http.MethodDelete:
		_, _ = w.Write([]byte(`{"id":"x","object":"invoiceitem","deleted":true}`))
	case r.URL.Path == "/v1/invoices/in_1/finalize":
		_, _ = w.Write([]byte(`{"id":"in_1","object":"invoice","status":"open","number":"DATUM-0042",
		  "hosted_invoice_url":"https://invoice.stripe.com/i/in_1","amount_remaining":63000,
		  "status_transitions":{"finalized_at":1796083200}}`))
	case r.URL.Path == "/v1/invoices/in_1" && r.Method == http.MethodPost:
		_, _ = w.Write([]byte(`{"id":"in_1","object":"invoice","status":"draft"}`))
	case r.URL.Path == "/v1/invoices/in_1" && r.Method == http.MethodGet:
		if f.invoice == "" {
			missing()
			return
		}
		_, _ = w.Write([]byte(f.invoice))
	case r.URL.Path == "/v1/invoices/in_1" && r.Method == http.MethodDelete:
		_, _ = w.Write([]byte(`{"id":"in_1","object":"invoice","deleted":true}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"unexpected ` + r.Method + ` ` + r.URL.Path + `"}}`))
	}
}

func (f *fakeInvoices) find(method, path string) []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRequest
	for _, r := range f.requests {
		if r.method == method && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func newInvoicesClient(t *testing.T, f *fakeInvoices) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newClientForBackend(t, srv.URL)
}

var (
	periodStart = time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2027, 1, 14, 23, 59, 59, 0, time.UTC)
)

func rackDraft() InvoiceDraft {
	return InvoiceDraft{
		CustomerID:     "cus_1",
		Currency:       "USD",
		Description:    "Preview only",
		IdempotencyKey: "stripeinvoice-uid-1",
		Metadata:       map[string]string{MetadataStripeInvoiceUID: "uid-1", MetadataPurpose: "PreviewDraft"},
		Lines: []InvoiceLine{
			{
				Key: "rack", Description: "Rack space", ProductID: "datum_rack",
				Quantity: decimal.RequireFromString("1.5"), UnitAmount: decimal.RequireFromString("330.00"),
				Start: periodStart, End: periodEnd, TaxCode: "txcd_20030000",
				DiscountPercent: decimal.RequireFromString("10"), Note: "Install delayed a week",
				Metadata: map[string]string{"service_pricing": "rack-v1"},
			},
			{
				Key: "setup", Description: "Setup", ProductID: "datum_setup",
				Quantity: decimal.NewFromInt(1), UnitAmount: decimal.RequireFromString("135.00"),
				Start: periodStart, End: periodStart, TaxCode: NontaxableTaxCode,
			},
		},
	}
}

func TestPreviewInvoice(t *testing.T) {
	f := &fakeInvoices{coupons: map[string]bool{}}
	res, err := newInvoicesClient(t, f).PreviewInvoice(context.Background(), rackDraft())
	if err != nil {
		t.Fatalf("PreviewInvoice: %v", err)
	}

	previews := f.find(http.MethodPost, "/v1/invoices/create_preview")
	if len(previews) != 1 {
		t.Fatalf("want one preview, got %d", len(previews))
	}
	form := previews[0].form
	for k, want := range map[string]string{
		"customer":                              "cus_1",
		"currency":                              "usd",
		"automatic_tax[enabled]":                "true",
		"invoice_items[0][price_data][product]": "datum_rack",
		"invoice_items[0][price_data][unit_amount_decimal]": "33000",
		"invoice_items[0][price_data][tax_behavior]":        "exclusive",
		"invoice_items[0][quantity_decimal]":                "1.5",
		"invoice_items[0][tax_code]":                        "txcd_20030000",
		"invoice_items[0][period][start]":                   "1797292800",
		"invoice_items[0][discounts][0][coupon]":            "datum-pct-10",
		"invoice_items[0][metadata][datum_line_key]":        "rack",
		"invoice_items[0][metadata][datum_note]":            "Install delayed a week",
		"invoice_items[0][metadata][datum_service_pricing]": "rack-v1",
		"invoice_items[1][tax_code]":                        NontaxableTaxCode,
	} {
		if got := form.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(f.find(http.MethodPost, "/v1/coupons")) != 1 {
		t.Error("the 10% coupon should be created on first use")
	}
	if len(f.find(http.MethodPost, "/v1/invoices")) != 0 || len(f.find(http.MethodPost, "/v1/invoiceitems")) != 0 {
		t.Error("a preview must create nothing")
	}

	if res.Subtotal != 63000 || res.Discount != 4950 || res.Tax != 3465 || res.Total != 61515 {
		t.Errorf("totals = %+v", res)
	}
	if res.StartingBalance != 23 || res.AmountDue != 61538 {
		t.Errorf("starting balance %d, amount due %d", res.StartingBalance, res.AmountDue)
	}
	if len(res.Lines) != 2 || res.Lines[0] != (LineResult{Key: "rack", Amount: 49500, Discount: 4950, Tax: 3465}) ||
		res.Lines[1].Key != "setup" {
		t.Errorf("lines = %+v", res.Lines)
	}
}

func TestEnsureDraftInvoice_CreatesTheDraftWithItsLines(t *testing.T) {
	f := &fakeInvoices{
		coupons: map[string]bool{"datum-pct-10": true},
		drafts:  `{"object":"list","data":[{"id":"in_other","object":"invoice","metadata":{"datum_stripe_invoice_uid":"someone-else"}}],"has_more":false}`,
		lines:   `{"object":"list","data":[],"has_more":false}`,
		invoice: `{"id":"in_1","object":"invoice","status":"draft","subtotal":63000,"total":63000,"lines":{"object":"list","data":[],"has_more":false}}`,
	}
	res, err := newInvoicesClient(t, f).EnsureDraftInvoice(context.Background(), "", rackDraft())
	if err != nil {
		t.Fatalf("EnsureDraftInvoice: %v", err)
	}
	if res.ID != "in_1" {
		t.Errorf("id = %q", res.ID)
	}

	creates := f.find(http.MethodPost, "/v1/invoices")
	if len(creates) != 1 {
		t.Fatalf("want one invoice created, got %d", len(creates))
	}
	for k, want := range map[string]string{
		"customer":                       "cus_1",
		"auto_advance":                   "false",
		"pending_invoice_items_behavior": "exclude",
		"automatic_tax[enabled]":         "true",
		"description":                    "Preview only",
		"metadata[datum_purpose]":        "PreviewDraft",
	} {
		if got := creates[0].form.Get(k); got != want {
			t.Errorf("invoice %s = %q, want %q", k, got, want)
		}
	}
	if creates[0].idempotency != "stripeinvoice-uid-1-invoice" {
		t.Errorf("idempotency key = %q", creates[0].idempotency)
	}

	items := f.find(http.MethodPost, "/v1/invoiceitems")
	if len(items) != 2 {
		t.Fatalf("want two items, got %d", len(items))
	}
	for _, item := range items {
		// Never left pending on the customer, where a real invoice could
		// pick it up.
		if item.form.Get("invoice") != "in_1" {
			t.Errorf("item not on the draft: %v", item.form)
		}
	}
	if !strings.HasPrefix(items[0].idempotency, "stripeinvoice-uid-1-line-rack-") {
		t.Errorf("item idempotency key = %q, want the line's key and hash", items[0].idempotency)
	}
	if items[0].form.Get("metadata[datum_line_hash]") == "" {
		t.Errorf("item has no line hash: %v", items[0].form)
	}
}

func invoiceDraft() InvoiceDraft {
	d := rackDraft()
	d.Description = ""
	d.Metadata = map[string]string{MetadataStripeInvoiceUID: "uid-1", MetadataPurpose: "Invoice"}
	d.CollectionMethod = CollectionSendInvoice
	d.DaysUntilDue = 45
	return d
}

func TestEnsureDraftInvoice_SendsAnInvoiceOnPaymentTerms(t *testing.T) {
	f := &fakeInvoices{
		coupons: map[string]bool{"datum-pct-10": true},
		drafts:  `{"object":"list","data":[],"has_more":false}`,
		lines:   `{"object":"list","data":[],"has_more":false}`,
		invoice: `{"id":"in_1","object":"invoice","status":"draft","collection_method":"send_invoice","created":1796083200,"due_date":1799971200}`,
	}
	if _, err := newInvoicesClient(t, f).EnsureDraftInvoice(context.Background(), "", invoiceDraft()); err != nil {
		t.Fatalf("EnsureDraftInvoice: %v", err)
	}
	create := f.find(http.MethodPost, "/v1/invoices")[0]
	if create.form.Get("collection_method") != "send_invoice" || create.form.Get("days_until_due") != "45" {
		t.Errorf("collection = %q due in %q days, want send_invoice in 45", create.form.Get("collection_method"), create.form.Get("days_until_due"))
	}
	if create.form.Has("description") {
		t.Errorf("a real invoice has no preview memo: %q", create.form.Get("description"))
	}
	if n := len(f.find(http.MethodPost, "/v1/invoices/in_1")); n != 0 {
		t.Errorf("collection already right, but updated %d times", n)
	}
}

func TestEnsureDraftInvoice_RewritesChangedLinesAndRemovesOldOnes(t *testing.T) {
	d := invoiceDraft()
	rackHash := d.Lines[0].hash()
	f := &fakeInvoices{
		coupons: map[string]bool{"datum-pct-10": true},
		lines: `{"object":"list","has_more":false,"data":[
		  {"id":"il_1","object":"line_item","metadata":{"datum_line_key":"rack","datum_line_hash":"` + rackHash + `"},
		   "parent":{"type":"invoice_item_details","invoice_item_details":{"invoice_item":"ii_rack"}}},
		  {"id":"il_2","object":"line_item","metadata":{"datum_line_key":"setup","datum_line_hash":"stale"},
		   "parent":{"type":"invoice_item_details","invoice_item_details":{"invoice_item":"ii_setup"}}},
		  {"id":"il_3","object":"line_item","metadata":{"datum_line_key":"gone","datum_line_hash":"x"},
		   "parent":{"type":"invoice_item_details","invoice_item_details":{"invoice_item":"ii_gone"}}}]}`,
		invoice: `{"id":"in_1","object":"invoice","status":"draft","collection_method":"charge_automatically"}`,
	}
	if _, err := newInvoicesClient(t, f).EnsureDraftInvoice(context.Background(), "in_1", d); err != nil {
		t.Fatalf("EnsureDraftInvoice: %v", err)
	}
	var deleted []string
	for _, r := range f.requests {
		if r.method == http.MethodDelete {
			deleted = append(deleted, r.path)
		}
	}
	if strings.Join(deleted, ",") != "/v1/invoiceitems/ii_setup,/v1/invoiceitems/ii_gone" {
		t.Errorf("deleted %v, want the changed and the removed line", deleted)
	}
	items := f.find(http.MethodPost, "/v1/invoiceitems")
	if len(items) != 1 || items[0].form.Get("metadata[datum_line_key]") != "setup" {
		t.Errorf("want only the changed line written again, got %v", items)
	}
	update := f.find(http.MethodPost, "/v1/invoices/in_1")
	if len(update) != 1 || update[0].form.Get("collection_method") != "send_invoice" || update[0].form.Get("days_until_due") != "45" {
		t.Errorf("collection not moved to payment terms: %v", update)
	}
}

func TestFinalizeInvoice(t *testing.T) {
	f := &fakeInvoices{invoice: `{"id":"in_1","object":"invoice","status":"draft"}`}
	res, err := newInvoicesClient(t, f).FinalizeInvoice(context.Background(), "in_1", "stripeinvoice-uid-1")
	if err != nil {
		t.Fatalf("FinalizeInvoice: %v", err)
	}
	calls := f.find(http.MethodPost, "/v1/invoices/in_1/finalize")
	if len(calls) != 1 || calls[0].form.Get("auto_advance") != "true" {
		t.Fatalf("finalize calls = %v, want one with auto_advance so Stripe charges or sends it", calls)
	}
	if res.Status != "open" || res.Number != "DATUM-0042" || res.HostedInvoiceURL == "" || res.FinalizedAt.IsZero() {
		t.Errorf("result = %+v", res)
	}

	// Already finalized: just read.
	f = &fakeInvoices{invoice: `{"id":"in_1","object":"invoice","status":"paid","number":"DATUM-0042"}`}
	res, err = newInvoicesClient(t, f).FinalizeInvoice(context.Background(), "in_1", "stripeinvoice-uid-1")
	if err != nil || res.Status != "paid" || len(f.find(http.MethodPost, "/v1/invoices/in_1/finalize")) != 0 {
		t.Errorf("finalizing a paid invoice: %+v, %v", res, err)
	}
}

func TestRetrieveInvoice_Missing(t *testing.T) {
	_, err := newInvoicesClient(t, &fakeInvoices{}).RetrieveInvoice(context.Background(), "in_1")
	if !errors.Is(err, ErrInvoiceMissing) {
		t.Errorf("err = %v, want ErrInvoiceMissing", err)
	}
}

func TestEnsureDraftInvoice_FinishesADraftAnEarlierAttemptMade(t *testing.T) {
	f := &fakeInvoices{
		coupons: map[string]bool{"datum-pct-10": true},
		drafts:  `{"object":"list","data":[{"id":"in_1","object":"invoice","metadata":{"datum_stripe_invoice_uid":"uid-1"}}],"has_more":false}`,
		lines:   `{"object":"list","data":[{"id":"il_1","object":"line_item","metadata":{"datum_line_key":"rack"}}],"has_more":false}`,
		invoice: `{"id":"in_1","object":"invoice","status":"draft","lines":{"object":"list","data":[],"has_more":false}}`,
	}
	if _, err := newInvoicesClient(t, f).EnsureDraftInvoice(context.Background(), "", rackDraft()); err != nil {
		t.Fatalf("EnsureDraftInvoice: %v", err)
	}
	if n := len(f.find(http.MethodPost, "/v1/invoices")); n != 0 {
		t.Errorf("adopted draft re-created %d times", n)
	}
	items := f.find(http.MethodPost, "/v1/invoiceitems")
	if len(items) != 1 || items[0].form.Get("metadata[datum_line_key]") != "setup" {
		t.Errorf("only the missing line should be added, got %v", items)
	}
}

func TestDeleteDraftInvoice(t *testing.T) {
	t.Run("deletes a draft", func(t *testing.T) {
		f := &fakeInvoices{invoice: `{"id":"in_1","object":"invoice","status":"draft"}`}
		if err := newInvoicesClient(t, f).DeleteDraftInvoice(context.Background(), "in_1"); err != nil {
			t.Fatalf("DeleteDraftInvoice: %v", err)
		}
		if len(f.find(http.MethodDelete, "/v1/invoices/in_1")) != 1 {
			t.Error("draft not deleted")
		}
	})
	t.Run("one already gone is fine", func(t *testing.T) {
		if err := newInvoicesClient(t, &fakeInvoices{}).DeleteDraftInvoice(context.Background(), "in_1"); err != nil {
			t.Fatalf("DeleteDraftInvoice: %v", err)
		}
	})
	t.Run("a finalized one is reported, not deleted", func(t *testing.T) {
		f := &fakeInvoices{invoice: `{"id":"in_1","object":"invoice","status":"open"}`}
		err := newInvoicesClient(t, f).DeleteDraftInvoice(context.Background(), "in_1")
		if !errors.Is(err, ErrInvoiceNotDraft) {
			t.Fatalf("err = %v, want ErrInvoiceNotDraft", err)
		}
		if len(f.find(http.MethodDelete, "/v1/invoices/in_1")) != 0 {
			t.Error("a finalized invoice must not be deleted")
		}
	})
}

func TestDashboardInvoiceURL(t *testing.T) {
	if got := (&Client{testMode: true}).DashboardInvoiceURL("in_1"); got != "https://dashboard.stripe.com/test/invoices/in_1" {
		t.Errorf("test mode url = %s", got)
	}
	if got := (&Client{}).DashboardInvoiceURL("in_1"); got != "https://dashboard.stripe.com/invoices/in_1" {
		t.Errorf("live url = %s", got)
	}
}
