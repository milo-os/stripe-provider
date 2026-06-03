// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"fmt"
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

// TestApplyCustomerDetails_StampsOrganizationsAndProjects confirms the
// org-list and project-list lands on Customer.metadata as comma-separated
// strings, that empty inputs clear the keys, and that a list past the
// 500-char Stripe per-value cap is truncated with an ellipsis marker on
// a name boundary (no half-names on the wire).
func TestApplyCustomerDetails_StampsOrganizationsAndProjects(t *testing.T) {
	t.Run("renders projects + organizations as comma-separated metadata", func(t *testing.T) {
		params := newParams(nil)
		applyCustomerDetails(params, CustomerDetails{
			Organizations: []string{"organization-chips-coding-tuzu8j"},
			Projects:      []string{"matt-jenkinson-yz0y92", "bug-buddy-selzs2"},
		})
		if got := params.Metadata["organizations"]; got != "organization-chips-coding-tuzu8j" {
			t.Errorf("organizations: want %q, got %q", "organization-chips-coding-tuzu8j", got)
		}
		if got := params.Metadata["projects"]; got != "matt-jenkinson-yz0y92, bug-buddy-selzs2" {
			t.Errorf("projects: want %q, got %q", "matt-jenkinson-yz0y92, bug-buddy-selzs2", got)
		}
	})

	t.Run("empty lists stamp empty strings to clear the keys", func(t *testing.T) {
		params := newParams(nil)
		applyCustomerDetails(params, CustomerDetails{})
		if v, ok := params.Metadata["organizations"]; !ok || v != "" {
			t.Errorf("organizations: want present and empty, got ok=%v v=%q", ok, v)
		}
		if v, ok := params.Metadata["projects"]; !ok || v != "" {
			t.Errorf("projects: want present and empty, got ok=%v v=%q", ok, v)
		}
	})

	t.Run("truncates oversized project lists on a name boundary", func(t *testing.T) {
		// Construct enough projects to overflow the 500-char Stripe
		// per-value cap. ~30 chars per name × 25 names overflows easily.
		names := make([]string, 25)
		for i := range names {
			names[i] = fmt.Sprintf("project-with-a-long-name-%02d", i)
		}
		params := newParams(nil)
		applyCustomerDetails(params, CustomerDetails{Projects: names})
		got := params.Metadata["projects"]
		if len(got) > 500 {
			t.Fatalf("truncated value still exceeds 500 chars: len=%d", len(got))
		}
		if !strings.HasSuffix(got, ", …") {
			t.Errorf("expected ellipsis suffix on truncated list, got tail %q", got[max(0, len(got)-10):])
		}
		// Truncation must happen on a name boundary — the last name we
		// included must be complete, not chopped mid-string.
		head := strings.TrimSuffix(got, ", …")
		parts := strings.Split(head, ", ")
		last := parts[len(parts)-1]
		matched := false
		for _, name := range names {
			if name == last {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("last entry %q is not a complete project name from the input list", last)
		}
	})
}

// TestApplyCustomerDetails_StampsDefaultPaymentMethod confirms the
// default-PM ID lands on Customer.invoice_settings.default_payment_method
// — i.e. the field Stripe's auto-invoice machinery reads. Empty input
// must still result in a populated InvoiceSettings struct so the upstream
// default gets cleared rather than left stale.
func TestApplyCustomerDetails_StampsDefaultPaymentMethod(t *testing.T) {
	t.Run("non-empty default flows to invoice_settings.default_payment_method", func(t *testing.T) {
		params := newParams(nil)
		applyCustomerDetails(params, CustomerDetails{
			DefaultPaymentMethodID: "pm_1AbCdEfGhIjKlMnO",
		})
		if params.InvoiceSettings == nil {
			t.Fatalf("InvoiceSettings should be set, not nil")
		}
		if got := params.InvoiceSettings.DefaultPaymentMethod; got == nil || *got != "pm_1AbCdEfGhIjKlMnO" {
			t.Errorf("DefaultPaymentMethod: want pm_1AbCdEfGhIjKlMnO, got %v", got)
		}
	})

	t.Run("empty default still sets the field so Stripe clears its value", func(t *testing.T) {
		params := newParams(nil)
		applyCustomerDetails(params, CustomerDetails{
			DefaultPaymentMethodID: "",
		})
		if params.InvoiceSettings == nil {
			t.Fatalf("InvoiceSettings should be set even when DefaultPaymentMethodID is empty; got nil")
		}
		if got := params.InvoiceSettings.DefaultPaymentMethod; got == nil || *got != "" {
			t.Errorf("DefaultPaymentMethod: want pointer to empty string for clear semantics, got %v", got)
		}
	})
}

// TestEnsureCustomer_UpdatePathSendsDefaultPaymentMethod confirms the
// form-encoded body Stripe receives carries
// invoice_settings[default_payment_method]= on the update path.
func TestEnsureCustomer_UpdatePathSendsDefaultPaymentMethod(t *testing.T) {
	var postBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/customers/cus_default"):
			body, _ := io.ReadAll(r.Body)
			postBody.Store(string(body))
			_, _ = w.Write([]byte(`{"id":"cus_default","name":"Acme Ltd"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tax_ids"):
			_, _ = w.Write([]byte(`{"object":"list","data":[],"has_more":false,"url":"/v1/customers/cus_default/tax_ids"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newClientForBackend(t, srv.URL)

	_, err := c.EnsureCustomer(context.Background(), "cus_default", "ba-default", CustomerDetails{
		DefaultPaymentMethodID: "pm_9XyZAbcDef",
	})
	if err != nil {
		t.Fatalf("EnsureCustomer update path: %v", err)
	}
	body, _ := postBody.Load().(string)
	decoded, _ := url.QueryUnescape(body)
	if !strings.Contains(decoded, "invoice_settings[default_payment_method]=pm_9XyZAbcDef") {
		t.Errorf("update body missing invoice_settings[default_payment_method]=pm_9XyZAbcDef\nbody: %s", decoded)
	}
}
