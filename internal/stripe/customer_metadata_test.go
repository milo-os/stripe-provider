// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	stripego "github.com/stripe/stripe-go/v81"
)

// TestApplyCustomerDetails_StampsMetadata exercises the post-merge
// metadata behaviour from applyCustomerDetails directly (no Stripe
// round-trip). Confirms that:
//   - business_name and individual_name are stamped from CustomerDetails
//   - existing metadata keys are preserved (so Customer.New's
//     billing_account dedup tag isn't clobbered)
//   - empty values are stamped as empty strings — Stripe interprets
//     this as "delete the key", which is the desired clear behaviour
func TestApplyCustomerDetails_StampsMetadata(t *testing.T) {
	t.Run("populates business + individual from CustomerDetails", func(t *testing.T) {
		params := newParams(nil)
		applyCustomerDetails(params, CustomerDetails{
			Name:           "Acme Ltd",
			BusinessName:   "Acme Ltd",
			IndividualName: "Matt Jenkinson",
		})
		got := params.Metadata
		if got["business_name"] != "Acme Ltd" {
			t.Errorf("business_name: want %q, got %q", "Acme Ltd", got["business_name"])
		}
		if got["individual_name"] != "Matt Jenkinson" {
			t.Errorf("individual_name: want %q, got %q", "Matt Jenkinson", got["individual_name"])
		}
	})

	t.Run("preserves billing_account when present on create path", func(t *testing.T) {
		// On the create path EnsureCustomer pre-seeds metadata with
		// billing_account for the customer-search dedup. The stamp
		// below must not drop that key.
		params := newParams(map[string]string{
			"billing_account": "ba-existing-1",
		})
		applyCustomerDetails(params, CustomerDetails{
			BusinessName:   "Acme Ltd",
			IndividualName: "Matt Jenkinson",
		})
		got := params.Metadata
		if got["billing_account"] != "ba-existing-1" {
			t.Errorf("billing_account should be preserved, got %q", got["billing_account"])
		}
		if got["business_name"] != "Acme Ltd" {
			t.Errorf("business_name: want %q, got %q", "Acme Ltd", got["business_name"])
		}
	})

	t.Run("stamps empty strings when fields are unset", func(t *testing.T) {
		// Sending metadata["key"] = "" tells Stripe to delete the
		// key. That's the right behaviour when the user removes the
		// business name from the BillingAccount — the next reconcile
		// should clear it from Stripe rather than leave stale data.
		params := newParams(nil)
		applyCustomerDetails(params, CustomerDetails{})
		got := params.Metadata
		if v, ok := got["business_name"]; !ok || v != "" {
			t.Errorf("business_name should be present and empty for clear semantics, got ok=%v v=%q", ok, v)
		}
		if v, ok := got["individual_name"]; !ok || v != "" {
			t.Errorf("individual_name should be present and empty for clear semantics, got ok=%v v=%q", ok, v)
		}
	})
}

// TestEnsureCustomer_UpdatePathSendsMetadata wires applyCustomerDetails
// through EnsureCustomer's update path with an httptest backend so we
// see the exact form-encoded body Stripe receives. The
// metadata[business_name]= keys must be on the wire for the dashboard
// to reflect them.
func TestEnsureCustomer_UpdatePathSendsMetadata(t *testing.T) {
	var (
		updatePosts atomic.Int32
		postBody    atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/customers/cus_existing"):
			updatePosts.Add(1)
			body, _ := io.ReadAll(r.Body)
			postBody.Store(string(body))
			_, _ = w.Write([]byte(`{"id":"cus_existing","name":"Acme Ltd","metadata":{"billing_account":"ba-existing","business_name":"Acme Ltd","individual_name":"Matt Jenkinson"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tax_ids"):
			// EnsureCustomer's update path follows the Customer.Update
			// with reconcileTaxIDs, which lists tax_ids before diffing.
			// We don't care about the list here — return an empty page
			// so the loop falls through and the test can complete.
			_, _ = w.Write([]byte(`{"object":"list","data":[],"has_more":false,"url":"/v1/customers/cus_existing/tax_ids"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newClientForBackend(t, srv.URL)

	_, err := c.EnsureCustomer(context.Background(), "cus_existing", "ba-existing", CustomerDetails{
		Name:           "Acme Ltd",
		BusinessName:   "Acme Ltd",
		IndividualName: "Matt Jenkinson",
	})
	if err != nil {
		t.Fatalf("EnsureCustomer update path: %v", err)
	}
	if updatePosts.Load() < 1 {
		t.Fatalf("expected at least 1 POST (the Customers.Update call)")
	}
	body, _ := postBody.Load().(string)

	// Stripe's form-encoded request body uses bracketed nested keys,
	// e.g. metadata[business_name]=Acme+Ltd. URL-decode for sanity but
	// match against both encoded and decoded forms.
	decoded, _ := url.QueryUnescape(body)
	for _, want := range []string{
		"metadata[business_name]=Acme Ltd",
		"metadata[individual_name]=Matt Jenkinson",
	} {
		if !strings.Contains(decoded, want) {
			t.Errorf("update body missing %q\nbody: %s", want, decoded)
		}
	}
}

func newParams(initialMetadata map[string]string) *stripego.CustomerParams {
	return &stripego.CustomerParams{
		Metadata: initialMetadata,
	}
}
