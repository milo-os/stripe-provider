// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeProducts serves Stripe's product retrieve, create and update.
// existing is the Product's JSON when it exists. createConflict makes the
// create report the ID taken, as when an earlier create's response was
// lost, and makes the Product exist.
type fakeProducts struct {
	mu             sync.Mutex
	existing       string
	createConflict bool
	creates        []url.Values
	updates        []url.Values
}

func (f *fakeProducts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/products/prod_datum_1":
		if f.existing == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"resource_missing","type":"invalid_request_error","message":"No such product"}}`))
			return
		}
		_, _ = w.Write([]byte(f.existing))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/products":
		f.creates = append(f.creates, form)
		if f.createConflict {
			f.existing = `{"id":"prod_datum_1","object":"product","name":"Old name","active":true,"metadata":{}}`
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"resource_already_exists","type":"invalid_request_error","message":"Product already exists"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": form.Get("id"), "object": "product"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/products/prod_datum_1":
		f.updates = append(f.updates, form)
		_, _ = w.Write([]byte(`{"id":"prod_datum_1","object":"product"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"unexpected ` + r.Method + ` ` + r.URL.Path + `"}}`))
	}
}

func newProductsClient(t *testing.T, f *fakeProducts) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newClientForBackend(t, srv.URL)
}

var cabling = ProductDetails{
	ID:       "prod_datum_1",
	Name:     "Structured cabling",
	TaxCode:  "txcd_20030000",
	Active:   true,
	Metadata: ProductMetadata("milo-system", "sku-dc-cab-01-abc123", "DC-CAB-01"),
}

func TestEnsureProduct_Creates(t *testing.T) {
	f := &fakeProducts{}
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), cabling); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("want one create, got %d", len(f.creates))
	}
	for k, want := range map[string]string{
		"id":                             "prod_datum_1",
		"name":                           "Structured cabling",
		"tax_code":                       "txcd_20030000",
		"active":                         "true",
		"metadata[datum_sku]":            "DC-CAB-01",
		"metadata[datum_stripe_product]": "milo-system/sku-dc-cab-01-abc123",
	} {
		if got := f.creates[0].Get(k); got != want {
			t.Errorf("create %s = %q, want %q", k, got, want)
		}
	}
}

func TestEnsureProduct_LeavesAMatchingProductAlone(t *testing.T) {
	f := &fakeProducts{existing: `{"id":"prod_datum_1","object":"product","name":"Structured cabling","active":true,
		"tax_code":{"id":"txcd_20030000","object":"tax_code"},
		"metadata":{"datum_sku":"DC-CAB-01","datum_stripe_product":"milo-system/sku-dc-cab-01-abc123","datum_managed_by":"stripe-provider","finance_note":"kept"}}`}
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), cabling); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if len(f.creates)+len(f.updates) != 0 {
		t.Errorf("want no writes, got creates %v updates %v", f.creates, f.updates)
	}
}

func TestEnsureProduct_UpdatesWhatDiffers(t *testing.T) {
	f := &fakeProducts{existing: `{"id":"prod_datum_1","object":"product","name":"Cabling","active":true,
		"tax_code":{"id":"txcd_10000000","object":"tax_code"},
		"metadata":{"datum_sku":"DC-CAB-01","datum_stripe_product":"milo-system/sku-dc-cab-01-abc123","datum_managed_by":"stripe-provider"}}`}
	d := cabling
	d.Active = false
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), d); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("want one update, got %d", len(f.updates))
	}
	got := f.updates[0]
	for k, want := range map[string]string{"name": "Structured cabling", "tax_code": "txcd_20030000", "active": "false"} {
		if got.Get(k) != want {
			t.Errorf("update %s = %q, want %q", k, got.Get(k), want)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, "metadata") {
			t.Errorf("unchanged metadata sent: %s", k)
		}
	}
}

func TestEnsureProduct_ClearsATaxCode(t *testing.T) {
	f := &fakeProducts{existing: `{"id":"prod_datum_1","object":"product","name":"Structured cabling","active":true,
		"tax_code":{"id":"txcd_20030000","object":"tax_code"},"metadata":{}}`}
	d := cabling
	d.TaxCode = ""
	d.Metadata = nil
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), d); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("want one update, got %d", len(f.updates))
	}
	if v, ok := f.updates[0]["tax_code"]; !ok || len(v) != 1 || v[0] != "" {
		t.Errorf("update should unset tax_code, got %v", f.updates[0])
	}
}

func TestEnsureProduct_AdoptsTheProductALostCreateMade(t *testing.T) {
	f := &fakeProducts{createConflict: true}
	// The first retrieve misses, the create finds the ID taken, and the
	// Product is brought in line.
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), cabling); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if len(f.creates) != 1 || len(f.updates) != 1 {
		t.Fatalf("want one create and one update, got %d and %d", len(f.creates), len(f.updates))
	}
	if f.updates[0].Get("name") != "Structured cabling" {
		t.Errorf("update = %v", f.updates[0])
	}
}

func TestEnsureProduct_Description(t *testing.T) {
	describe := func(s string) *string { return &s }

	f := &fakeProducts{}
	d := cabling
	d.Description = describe("Structured cabling within a cage")
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), d); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if got := f.creates[0].Get("description"); got != "Structured cabling within a cage" {
		t.Errorf("create description = %q", got)
	}

	existing := `{"id":"prod_datum_1","object":"product","name":"Structured cabling","active":true,"description":"Old words",
		"tax_code":{"id":"txcd_20030000","object":"tax_code"},"metadata":{}}`
	f = &fakeProducts{existing: existing}
	d = cabling
	d.Metadata = nil
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), d); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if len(f.updates) != 0 {
		t.Errorf("a nil description changed the Product: %v", f.updates)
	}

	d.Description = describe("")
	if err := newProductsClient(t, f).EnsureProduct(context.Background(), d); err != nil {
		t.Fatalf("EnsureProduct: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("want one update, got %d", len(f.updates))
	}
	if v, ok := f.updates[0]["description"]; !ok || len(v) != 1 || v[0] != "" {
		t.Errorf("update should unset description, got %v", f.updates[0])
	}
}
