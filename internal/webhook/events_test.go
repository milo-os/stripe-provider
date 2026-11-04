// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// recordedEvent is one Event a captureRecorder saw.
type recordedEvent struct {
	object      runtime.Object
	annotations map[string]string
	eventType   string
	reason      string
	message     string
}

// captureRecorder is a record.EventRecorder that keeps what it's given.
type captureRecorder struct {
	events []recordedEvent
}

func (c *captureRecorder) Event(object runtime.Object, eventType, reason, message string) {
	c.AnnotatedEventf(object, nil, eventType, reason, "%s", message)
}

func (c *captureRecorder) Eventf(object runtime.Object, eventType, reason, messageFmt string, args ...any) {
	c.AnnotatedEventf(object, nil, eventType, reason, messageFmt, args...)
}

func (c *captureRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventType, reason, messageFmt string, args ...any) {
	c.events = append(c.events, recordedEvent{
		object: object, annotations: annotations, eventType: eventType, reason: reason,
		message: fmt.Sprintf(messageFmt, args...),
	})
}

const eventsNamespace = "organization-acme"

func pendingPaymentMethod() (*billingv1alpha1.PaymentMethod, *stripev1alpha1.StripePaymentMethod) {
	pm := &billingv1alpha1.PaymentMethod{
		ObjectMeta: metav1.ObjectMeta{Name: "card-1", Namespace: eventsNamespace},
		Spec: billingv1alpha1.PaymentMethodSpec{
			BillingAccountRef:     billingv1alpha1.BillingAccountRef{Name: "acme"},
			DisplayName:           "Card",
			PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{Name: "stripe-default"},
		},
		Status: billingv1alpha1.PaymentMethodStatus{Phase: billingv1alpha1.PaymentMethodPhaseAwaitingConfirmation},
	}
	spm := &stripev1alpha1.StripePaymentMethod{
		ObjectMeta: metav1.ObjectMeta{Name: "card-1", Namespace: eventsNamespace},
		Spec:       stripev1alpha1.StripePaymentMethodSpec{PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: "card-1"}},
		Status: stripev1alpha1.StripePaymentMethodStatus{
			Phase:       stripev1alpha1.StripePaymentMethodPhaseAwaitingConfirmation,
			SetupIntent: &stripev1alpha1.StripeSetupIntentStatus{ID: "seti_1", Status: "requires_payment_method"},
		},
	}
	return pm, spm
}

func TestProjectOntoPaymentMethod_RecordsAttachedOnce(t *testing.T) {
	pm, spm := pendingPaymentMethod()
	wh, _ := newTestWebhook(t, nil, pm, spm)
	rec := &captureRecorder{}
	wh.Recorder = rec
	card := &stripeinternal.PaymentMethodDetails{
		ID: "pm_1", Type: "card", Brand: "visa", Last4: "4242", Country: "US", ExpMonth: 11, ExpYear: 2030,
	}

	// A second delivery of the same outcome (Stripe retries, or a later
	// event for the same SetupIntent) changes nothing and records nothing.
	for range 2 {
		if err := wh.projectOntoPaymentMethod(t.Context(), spm, card); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.events) != 1 {
		t.Fatalf("recorded %d events, want one PaymentMethodAttached", len(rec.events))
	}
	e := rec.events[0]
	if e.reason != billingv1alpha1.EventReasonPaymentMethodAttached || e.eventType != corev1.EventTypeNormal {
		t.Errorf("recorded %s %s, want Normal PaymentMethodAttached", e.eventType, e.reason)
	}
	want := map[string]string{
		billingv1alpha1.ScopeTypeAnnotation:           "Organization",
		billingv1alpha1.ScopeNameAnnotation:           "acme",
		billingv1alpha1.EventAnnotationBillingAccount: "acme",
		billingv1alpha1.EventAnnotationCardBrand:      "visa",
		billingv1alpha1.EventAnnotationCardLast4:      "4242",
		billingv1alpha1.EventAnnotationCardExpiry:     "2030-11-01T00:00:00Z",
	}
	if fmt.Sprint(e.annotations) != fmt.Sprint(want) {
		t.Errorf("annotations = %v, want %v", e.annotations, want)
	}
	if got, ok := e.object.(*billingv1alpha1.PaymentMethod); !ok || got.Name != "card-1" {
		t.Errorf("recorded on %v, want PaymentMethod card-1", e.object)
	}
}

func TestSetupIntentFailed_RecordsFailedOnceWithoutStripeDetail(t *testing.T) {
	pm, spm := pendingPaymentMethod()
	wh, _ := newTestWebhook(t, nil, pm, spm)
	rec := &captureRecorder{}
	wh.Recorder = rec

	deliver := func(eventID, eventType string) {
		t.Helper()
		body := []byte(`{
			"id":"` + eventID + `",
			"type":"` + eventType + `",
			"data":{"object":{
				"id":"seti_1",
				"status":"requires_payment_method",
				"last_setup_error":{"code":"card_declined","decline_code":"insufficient_funds","message":"Your card has insufficient funds."},
				"metadata":{"stripe_payment_method_namespace":"` + eventsNamespace + `","stripe_payment_method_name":"card-1"}
			}}
		}`)
		req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
		req.Header.Set("Stripe-Signature", stripeSignature(t, body, testWebhookSecret, time.Now()))
		w := httptest.NewRecorder()
		wh.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d (%s)", eventID, w.Code, w.Body.String())
		}
	}
	deliver("evt_failed", "setup_intent.setup_failed")
	// The SetupIntent is then canceled: still Failed, nothing new to say.
	deliver("evt_canceled", "setup_intent.canceled")

	if len(rec.events) != 1 {
		t.Fatalf("recorded %d events, want one PaymentMethodFailed", len(rec.events))
	}
	e := rec.events[0]
	if e.reason != billingv1alpha1.EventReasonPaymentMethodFailed || e.eventType != corev1.EventTypeWarning {
		t.Errorf("recorded %s %s, want Warning PaymentMethodFailed", e.eventType, e.reason)
	}
	if got := e.annotations[billingv1alpha1.EventAnnotationDeclineReason]; got != string(billingv1alpha1.DeclineReasonInsufficientFunds) {
		t.Errorf("decline reason = %q, want InsufficientFunds", got)
	}
	for k, v := range e.annotations {
		if strings.Contains(v, "card_declined") || strings.Contains(v, "insufficient_funds") || strings.Contains(v, "Your card") {
			t.Errorf("annotation %s carries Stripe's detail: %q", k, v)
		}
	}
	if strings.Contains(e.message, "Your card") {
		t.Errorf("message carries Stripe's text: %q", e.message)
	}
}

func TestSetupDeclineReason(t *testing.T) {
	for _, tc := range []struct {
		code, declineCode string
		want              billingv1alpha1.DeclineReason
	}{
		{"card_declined", "insufficient_funds", billingv1alpha1.DeclineReasonInsufficientFunds},
		{"expired_card", "", billingv1alpha1.DeclineReasonExpiredCard},
		{"card_declined", "expired_card", billingv1alpha1.DeclineReasonExpiredCard},
		{"setup_intent_authentication_failure", "", billingv1alpha1.DeclineReasonAuthenticationRequired},
		{"card_declined", "authentication_required", billingv1alpha1.DeclineReasonAuthenticationRequired},
		{"processing_error", "", billingv1alpha1.DeclineReasonProcessingError},
		{"card_declined", "try_again_later", billingv1alpha1.DeclineReasonProcessingError},
		{"card_declined", "do_not_honor", billingv1alpha1.DeclineReasonDeclined},
		{"incorrect_cvc", "", billingv1alpha1.DeclineReasonDeclined},
		{"", "", billingv1alpha1.DeclineReasonDeclined},
	} {
		if got := setupDeclineReason(tc.code, tc.declineCode); got != tc.want {
			t.Errorf("setupDeclineReason(%q, %q) = %s, want %s", tc.code, tc.declineCode, got, tc.want)
		}
	}
}

func TestFailedEventDetailWithoutError(t *testing.T) {
	pm, _ := pendingPaymentMethod()
	got := failedEventDetail(pm, &setupIntentPayload{ID: "seti_1"})
	if _, ok := got[billingv1alpha1.EventAnnotationDeclineReason]; ok || got[billingv1alpha1.EventAnnotationBillingAccount] != "acme" {
		t.Errorf("detail = %v, want only the billing account", got)
	}
}
