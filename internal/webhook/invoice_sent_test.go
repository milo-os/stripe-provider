// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

const sentAt = 1795166539 // 2026-11-20T09:22:19Z

func invoiceSentAccount() (*billingv1alpha1.BillingAccount, *stripev1alpha1.StripeCustomer) {
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "organization-acme", UID: "uid-acme"},
	}
	customer := &stripev1alpha1.StripeCustomer{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "organization-acme"},
		Spec:       stripev1alpha1.StripeCustomerSpec{BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: "acme"}},
		Status:     stripev1alpha1.StripeCustomerStatus{StripeCustomerID: "cus_acme"},
	}
	return account, customer
}

// invoiceSentBody is a signed-ready invoice.sent event. metadata is the
// invoice's metadata as a JSON object.
func invoiceSentBody(eventID, customer, metadata string) []byte {
	return fmt.Appendf(nil, `{"object":"event","id":%q,"type":"invoice.sent","created":%d,"data":{"object":{
		"id":"in_1","object":"invoice","number":"ACME-0042","customer":%q,"currency":"usd",
		"total":125000,"due_date":1797552000,"customer_email":"billing@example.com","metadata":%s}}}`,
		eventID, sentAt, customer, metadata)
}

func deliver(t *testing.T, wh *Webhook, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", stripeSignature(t, body, testWebhookSecret, time.Now()))
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	return rec.Code
}

func listEvents(t *testing.T, c client.Client) []corev1.Event {
	t.Helper()
	var events corev1.EventList
	if err := c.List(context.Background(), &events); err != nil {
		t.Fatalf("listing events: %v", err)
	}
	return events.Items
}

func TestInvoiceSent_RecordsOnTheBillingAccount(t *testing.T) {
	account, customer := invoiceSentAccount()
	wh, c := newTestWebhook(t, nil, account, customer)

	if code := deliver(t, wh, invoiceSentBody("evt_sent_1", "cus_acme", `{}`)); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	events := listEvents(t, c)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Reason != billingv1alpha1.EventReasonInvoiceSent || ev.Type != corev1.EventTypeNormal {
		t.Errorf("reason/type = %s/%s", ev.Reason, ev.Type)
	}
	if ev.Namespace != "organization-acme" || !strings.HasPrefix(ev.Name, "acme.") {
		t.Errorf("event %s/%s", ev.Namespace, ev.Name)
	}
	if ref := ev.InvolvedObject; ref.Kind != "BillingAccount" || ref.Name != "acme" || ref.UID != "uid-acme" ||
		ref.APIVersion != "billing.miloapis.com/v1alpha1" {
		t.Errorf("involvedObject = %+v", ref)
	}
	if !ev.FirstTimestamp.Equal(&metav1.Time{Time: time.Unix(sentAt, 0)}) || ev.Count != 1 {
		t.Errorf("firstTimestamp = %v, count = %d; want Stripe's created time", ev.FirstTimestamp, ev.Count)
	}
	if ev.ReportingController != eventRecorderName || ev.Source.Component != eventRecorderName {
		t.Errorf("reporting controller = %q, source = %q", ev.ReportingController, ev.Source.Component)
	}
	want := map[string]string{
		billingv1alpha1.ScopeTypeAnnotation:          "Organization",
		billingv1alpha1.ScopeNameAnnotation:          "acme",
		billingv1alpha1.EventAnnotationInvoiceNumber: "ACME-0042",
		billingv1alpha1.EventAnnotationCurrency:      "USD",
		billingv1alpha1.EventAnnotationAmount:        "1250.00",
		billingv1alpha1.EventAnnotationDueDate:       "2026-12-18T00:00:00Z",
	}
	if fmt.Sprint(ev.Annotations) != fmt.Sprint(want) {
		t.Errorf("annotations = %v, want %v", ev.Annotations, want)
	}
	for _, s := range append([]string{ev.Message}, mapValues(ev.Annotations)...) {
		if strings.Contains(s, "example.com") || strings.Contains(s, "in_1") || strings.Contains(s, "cus_") {
			t.Errorf("event carries Stripe detail: %q", s)
		}
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestInvoiceSent_NamesTheMiloInvoiceOnceProjected(t *testing.T) {
	account, customer := invoiceSentAccount()
	wh, c := newTestWebhook(t, nil, account, customer)

	body := invoiceSentBody("evt_sent_2", "cus_acme", `{"milo_invoice":"acme-2026-10","milo_billing_account":"acme"}`)
	if code := deliver(t, wh, body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	events := listEvents(t, c)
	if len(events) != 1 || events[0].Annotations[billingv1alpha1.EventAnnotationInvoice] != "acme-2026-10" {
		t.Fatalf("events = %+v, want one naming invoice acme-2026-10", events)
	}
}

func TestInvoiceSent_RedeliveryRecordsOnce(t *testing.T) {
	account, customer := invoiceSentAccount()
	// No dedupe, as after a restart or on another replica.
	wh, c := newTestWebhook(t, nil, account, customer)
	wh.Dedupe = nil

	body := invoiceSentBody("evt_sent_3", "cus_acme", `{}`)
	for i := range 2 {
		if code := deliver(t, wh, body); code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200", i+1, code)
		}
	}
	if n := len(listEvents(t, c)); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}

	// A re-send is another Stripe event, so another entry.
	if code := deliver(t, wh, invoiceSentBody("evt_sent_4", "cus_acme", `{}`)); code != http.StatusOK {
		t.Fatalf("re-send: status = %d", code)
	}
	if n := len(listEvents(t, c)); n != 2 {
		t.Fatalf("events after a re-send = %d, want 2", n)
	}
}

func TestInvoiceSent_IgnoresWhatIsntOurs(t *testing.T) {
	account, customer := invoiceSentAccount()
	for name, body := range map[string][]byte{
		"unknown customer": invoiceSentBody("evt_sent_5", "cus_other", `{}`),
		"preview draft":    invoiceSentBody("evt_sent_6", "cus_acme", `{"datum_purpose":"PreviewDraft"}`),
		"no customer":      invoiceSentBody("evt_sent_7", "", `{}`),
	} {
		t.Run(name, func(t *testing.T) {
			wh, c := newTestWebhook(t, nil, account.DeepCopy(), customer.DeepCopy())
			if code := deliver(t, wh, body); code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if n := len(listEvents(t, c)); n != 0 {
				t.Fatalf("events = %d, want 0", n)
			}
		})
	}
}

func TestInvoiceSent_MissingAccountIsIgnored(t *testing.T) {
	_, customer := invoiceSentAccount()
	wh, c := newTestWebhook(t, nil, customer)
	if code := deliver(t, wh, invoiceSentBody("evt_sent_8", "cus_acme", `{}`)); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if n := len(listEvents(t, c)); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}
}

func TestInvoiceSent_CreateErrorAsksStripeToRetry(t *testing.T) {
	account, customer := invoiceSentAccount()
	wh, c := newTestWebhook(t, nil, account, customer)
	wh.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Event); ok {
				return errors.New("apiserver unavailable")
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	if code := deliver(t, wh, invoiceSentBody("evt_sent_9", "cus_acme", `{}`)); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

func TestFormatMinorUnits(t *testing.T) {
	for _, tc := range []struct {
		amount   int64
		currency string
		want     string
	}{
		{125000, "usd", "1250.00"},
		{48219, "USD", "482.19"},
		{5, "eur", "0.05"},
		{0, "gbp", "0.00"},
		{1000, "jpy", "1000"},
	} {
		if got := formatMinorUnits(tc.amount, tc.currency); got != tc.want {
			t.Errorf("formatMinorUnits(%d, %s) = %q, want %q", tc.amount, tc.currency, got, tc.want)
		}
	}
}

func TestInvoiceSentEventName(t *testing.T) {
	a, b := invoiceSentEventName("acme", "evt_A"), invoiceSentEventName("acme", "evt_a")
	if a == b {
		t.Errorf("names for different events collide: %s", a)
	}
	if a != invoiceSentEventName("acme", "evt_A") {
		t.Errorf("name isn't stable")
	}
	if long := invoiceSentEventName(strings.Repeat("a", 300), "evt_A"); len(long) > 253 {
		t.Errorf("name is %d characters", len(long))
	}
}
