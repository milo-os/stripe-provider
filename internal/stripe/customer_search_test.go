// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeCustomerAPI serves just enough of Stripe's customer API for
// EnsureCustomer: search answers by query, and records each create and
// update body.
type fakeCustomerAPI struct {
	t *testing.T
	// search maps a customer search query to the customers JSON it
	// returns (the contents of `data`).
	search map[string]string

	mu      sync.Mutex
	queries []string
	creates []string
	updates map[string]string
}

func (f *fakeCustomerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/customers/search":
		q := r.URL.Query().Get("query")
		f.queries = append(f.queries, q)
		_, _ = w.Write([]byte(`{"object":"search_result","has_more":false,"data":[` + f.search[q] + `]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/customers":
		body, _ := io.ReadAll(r.Body)
		decoded, _ := url.QueryUnescape(string(body))
		f.creates = append(f.creates, decoded)
		_, _ = w.Write([]byte(`{"id":"cus_new"}`))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/customers/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/customers/")
		body, _ := io.ReadAll(r.Body)
		decoded, _ := url.QueryUnescape(string(body))
		if f.updates == nil {
			f.updates = map[string]string{}
		}
		f.updates[id] = decoded
		_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tax_ids"):
		_, _ = w.Write([]byte(`{"object":"list","data":[],"has_more":false,"url":"` + r.URL.Path + `"}`))
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}
}

func ensureAgainst(t *testing.T, api *fakeCustomerAPI, account BillingAccountKey) string {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	id, err := newClientForBackend(t, srv.URL).EnsureCustomer(context.Background(), "", account, CustomerDetails{})
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	return id
}

var acme = BillingAccountKey{Namespace: "organization-acme", Name: "ba-acme", UID: "uid-acme"}

const (
	uidQuery  = "metadata['billing_account_uid']:'uid-acme'"
	nameQuery = "metadata['billing_account']:'ba-acme'"
)

func TestEnsureCustomer_FindsCustomerByUID(t *testing.T) {
	api := &fakeCustomerAPI{t: t, search: map[string]string{
		uidQuery: `{"id":"cus_uid","metadata":{"billing_account":"ba-acme","billing_account_uid":"uid-acme"}}`,
	}}
	if id := ensureAgainst(t, api, acme); id != "cus_uid" {
		t.Fatalf("want cus_uid, got %q", id)
	}
	if len(api.creates) != 0 {
		t.Errorf("must not create a customer when one is tagged with the UID")
	}
	if len(api.queries) != 1 {
		t.Errorf("want only the UID search, got %q", api.queries)
	}
}

// Customers created before the UID was stamped only carry the account
// name, which another organization's account can share. Only the one
// whose organizations metadata names this account's namespace matches,
// and syncing it stamps the namespace and UID so the next search finds
// it by UID.
func TestEnsureCustomer_AdoptsLegacyCustomerFromTheSameNamespace(t *testing.T) {
	api := &fakeCustomerAPI{t: t, search: map[string]string{
		nameQuery: `{"id":"cus_other_org","metadata":{"billing_account":"ba-acme","organizations":"organization-other"}},` +
			`{"id":"cus_other_uid","metadata":{"billing_account":"ba-acme","billing_account_uid":"uid-old","organizations":"organization-acme"}},` +
			`{"id":"cus_legacy","metadata":{"billing_account":"ba-acme","organizations":"organization-shared, organization-acme"}}`,
	}}
	if id := ensureAgainst(t, api, acme); id != "cus_legacy" {
		t.Fatalf("want cus_legacy, got %q", id)
	}
	if len(api.creates) != 0 {
		t.Errorf("must not create a customer when a legacy one matches")
	}
	body := api.updates["cus_legacy"]
	for _, want := range []string{
		"metadata[billing_account]=ba-acme",
		"metadata[billing_account_namespace]=organization-acme",
		"metadata[billing_account_uid]=uid-acme",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("update body missing %q\nbody: %s", want, body)
		}
	}
}

func TestEnsureCustomer_CreatesTaggedCustomerWhenNoneMatches(t *testing.T) {
	api := &fakeCustomerAPI{t: t, search: map[string]string{
		nameQuery: `{"id":"cus_other_org","metadata":{"billing_account":"ba-acme","organizations":"organization-other"}}`,
	}}
	if id := ensureAgainst(t, api, acme); id != "cus_new" {
		t.Fatalf("want cus_new, got %q", id)
	}
	if len(api.creates) != 1 {
		t.Fatalf("want 1 create, got %d", len(api.creates))
	}
	for _, want := range []string{
		"metadata[billing_account]=ba-acme",
		"metadata[billing_account_namespace]=organization-acme",
		"metadata[billing_account_uid]=uid-acme",
	} {
		if !strings.Contains(api.creates[0], want) {
			t.Errorf("create body missing %q\nbody: %s", want, api.creates[0])
		}
	}
}
