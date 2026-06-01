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
	"sync/atomic"
	"testing"

	stripego "github.com/stripe/stripe-go/v81"
	stripeclient "github.com/stripe/stripe-go/v81/client"
)

// newClientForBackend constructs a *Client pointed at the supplied
// httptest server URL. It bypasses NewClient (which doesn't expose a
// URL override) by initializing the SDK's *client.API directly with a
// BackendConfig.URL set to the test server.
func newClientForBackend(t *testing.T, baseURL string) *Client {
	t.Helper()
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	noRetries := int64(0)
	cfg := &stripego.BackendConfig{
		URL:               stripego.String(parsed.String()),
		MaxNetworkRetries: &noRetries,
	}
	backends := &stripego.Backends{
		API:     stripego.GetBackendWithConfig(stripego.APIBackend, cfg),
		Uploads: stripego.GetBackendWithConfig(stripego.UploadsBackend, cfg),
	}
	api := &stripeclient.API{}
	api.Init("sk_test_dummy", backends)
	return &Client{api: api}
}

func TestBackfillCustomerName_NoOpOnEmptyInputs(t *testing.T) {
	// If either input is empty BackfillCustomerName must return nil
	// without touching the network. We assert this with a server that
	// fails the test on any inbound request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("did not expect any Stripe API call, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := newClientForBackend(t, srv.URL)

	for _, tc := range []struct {
		name       string
		customerID string
		value      string
	}{
		{"empty customer id", "", "Matt"},
		{"empty name", "cus_1", ""},
		{"both empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.BackfillCustomerName(context.Background(), tc.customerID, tc.value); err != nil {
				t.Fatalf("BackfillCustomerName: %v", err)
			}
		})
	}
}

func TestBackfillCustomerName_SkipsWhenCustomerAlreadyHasName(t *testing.T) {
	// The backfill must never overwrite a name a user has already
	// entered via the BillingAccount. We do that by checking
	// Customer.name on the Stripe-side record (single GET) and skipping
	// the POST when it is non-empty.
	var (
		gets  atomic.Int32
		posts atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "cus_existing",
				"name": "Acme Ltd", // already set by the user
			})
		case http.MethodPost:
			posts.Add(1)
			t.Errorf("must not POST update when Customer.name is already set; got body upload")
		}
	}))
	defer srv.Close()
	c := newClientForBackend(t, srv.URL)

	if err := c.BackfillCustomerName(context.Background(), "cus_existing", "Cardholder Name"); err != nil {
		t.Fatalf("BackfillCustomerName: %v", err)
	}
	if g := gets.Load(); g != 1 {
		t.Fatalf("expected exactly 1 GET, got %d", g)
	}
	if p := posts.Load(); p != 0 {
		t.Fatalf("expected 0 POSTs (skip), got %d", p)
	}
}

func TestBackfillCustomerName_UpdatesWhenCustomerNameEmpty(t *testing.T) {
	// Happy path: Customer.name is empty server-side and we have a
	// cardholder name in hand, so we issue exactly one update.
	var (
		gets     atomic.Int32
		postBody atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "cus_blank",
				"name": "",
			})
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			postBody.Store(string(body))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "cus_blank",
				"name": "Matt Jenkinson",
			})
		}
	}))
	defer srv.Close()
	c := newClientForBackend(t, srv.URL)

	if err := c.BackfillCustomerName(context.Background(), "cus_blank", "Matt Jenkinson"); err != nil {
		t.Fatalf("BackfillCustomerName: %v", err)
	}
	if g := gets.Load(); g != 1 {
		t.Fatalf("expected exactly 1 GET, got %d", g)
	}
	body, _ := postBody.Load().(string)
	if !strings.Contains(body, "name=Matt+Jenkinson") {
		t.Fatalf("expected POST body to contain name=Matt+Jenkinson, got %q", body)
	}
}

func TestBackfillCustomerName_IdempotentOnMissingCustomer(t *testing.T) {
	// resource_missing must be swallowed — the Customer was deleted
	// out from under us (race against teardown / manual cleanup), and
	// the webhook handler shouldn't loop on a 404.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_missing","message":"No such customer","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()
	c := newClientForBackend(t, srv.URL)

	if err := c.BackfillCustomerName(context.Background(), "cus_gone", "Matt"); err != nil {
		t.Fatalf("BackfillCustomerName must swallow resource_missing, got %v", err)
	}
}
