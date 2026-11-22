// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	"go.miloapis.com/stripe-provider/internal/controller"
)

func invoiceEventBody(eventID, kind, metadata string) []byte {
	return fmt.Appendf(nil, `{"object":"event","id":%q,"type":%q,"created":%d,"data":{"object":{
		"id":"in_1","object":"invoice","status":"paid","customer":"cus_other","currency":"usd","metadata":%s}}}`,
		eventID, kind, sentAt, metadata)
}

func TestInvoiceEvents_RefreshTheStripeInvoiceThatWroteIt(t *testing.T) {
	si := &stripev1alpha1.StripeInvoice{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-2026-12", Namespace: "organization-acme", UID: "uid-si"},
	}
	wh, c := newTestWebhook(t, nil, si)
	refreshed := func() string {
		var got stripev1alpha1.StripeInvoice
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(si), &got); err != nil {
			t.Fatalf("getting StripeInvoice: %v", err)
		}
		return got.Annotations[controller.StripeInvoiceRefreshAnnotation]
	}

	ours := `{"datum_purpose":"Invoice","datum_stripe_invoice":"organization-acme/acme-2026-12","datum_stripe_invoice_uid":"uid-si"}`
	if code := deliver(t, wh, invoiceEventBody("evt_paid", "invoice.paid", ours)); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := refreshed(); got != "evt_paid" {
		t.Errorf("refresh annotation = %q, want the event's id", got)
	}

	for _, md := range []string{
		`{"datum_purpose":"PreviewDraft","datum_stripe_invoice":"organization-acme/acme-2026-12","datum_stripe_invoice_uid":"uid-si"}`,
		`{"datum_purpose":"Invoice","datum_stripe_invoice":"organization-acme/acme-2026-12","datum_stripe_invoice_uid":"someone-else"}`,
		`{"datum_purpose":"Invoice","datum_stripe_invoice":"organization-acme/gone"}`,
		`{}`,
	} {
		if code := deliver(t, wh, invoiceEventBody("evt_other", "invoice.voided", md)); code != http.StatusOK {
			t.Fatalf("status = %d for %s, want 200", code, md)
		}
	}
	if got := refreshed(); got != "evt_paid" {
		t.Errorf("refresh annotation = %q after events for other invoices, want it untouched", got)
	}
}
