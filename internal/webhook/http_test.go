// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

const (
	testWebhookSecret  = "whsec_test_dummy_secret_value_xx"
	testProviderConfig = "test-stripe"
)

// stripeSignature builds a Stripe-compatible Stripe-Signature header
// for the given body + timestamp using HMAC-SHA256, matching the format
// the stripe-go webhook verifier expects.
func stripeSignature(t *testing.T, body []byte, secret string, ts time.Time) string {
	t.Helper()
	tsStr := fmt.Sprintf("%d", ts.Unix())
	payload := tsStr + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return "t=" + tsStr + ",v1=" + sig
}

func newTestWebhook(t *testing.T, dedupe EventDeduper, extra ...client.Object) (*Webhook, client.Client) {
	t.Helper()
	// The resolver reads Stripe SDK credentials from the controller-pod
	// environment, not the CRD. t.Setenv tears down at end-of-test.
	t.Setenv(stripeinternal.SecretKeyEnv, "sk_test_dummy")
	t.Setenv(stripeinternal.WebhookSecretEnv, testWebhookSecret)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering core scheme: %v", err)
	}
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering billing scheme: %v", err)
	}
	if err := stripev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering stripe scheme: %v", err)
	}

	cfg := &stripev1alpha1.StripeProviderConfig{}
	cfg.Name = testProviderConfig
	cfg.Spec.PublishableKey = "pk_test_dummy"

	objects := []client.Object{cfg}
	objects = append(objects, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(
		&stripev1alpha1.StripePaymentMethod{},
		&billingv1alpha1.PaymentMethod{},
	).Build()

	wh := NewStripeWebhook(c, testProviderConfig)
	if dedupe != nil {
		wh.Dedupe = dedupe
	}
	return wh, c
}

func TestWebhook_RejectsMissingSignature(t *testing.T) {
	wh, _ := newTestWebhook(t, nil)
	body := []byte(`{"id":"evt_1","type":"setup_intent.succeeded","data":{}}`)
	req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on missing signature, got %d", rec.Code)
	}
}

func TestWebhook_RejectsInvalidSignature(t *testing.T) {
	wh, _ := newTestWebhook(t, nil)
	body := []byte(`{"id":"evt_1","type":"setup_intent.succeeded","data":{"object":{"id":"seti_1"}}}`)
	req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid signature, got %d", rec.Code)
	}
}

func TestWebhook_RejectsNonPost(t *testing.T) {
	wh, _ := newTestWebhook(t, nil)
	req := httptest.NewRequest(http.MethodGet, Endpoint, nil)
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET, got %d", rec.Code)
	}
}

func TestWebhook_AcceptsValidSignedUnhandledEventNoOp(t *testing.T) {
	wh, _ := newTestWebhook(t, nil)
	body := []byte(`{"id":"evt_ignored","type":"customer.created","data":{"object":{"id":"cus_1"}}}`)
	sig := stripeSignature(t, body, testWebhookSecret, time.Now())
	req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unhandled event with valid signature, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestWebhook_DedupesRepeatedEventID(t *testing.T) {
	dedupe := stripeinternal.NewMemoryDeduper(0)
	wh, _ := newTestWebhook(t, dedupe)
	body := []byte(`{"id":"evt_dup","type":"customer.created","data":{"object":{"id":"cus_1"}}}`)
	sig := stripeSignature(t, body, testWebhookSecret, time.Now())

	req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first delivery: expected 200, got %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	req2.Header.Set("Stripe-Signature", sig)
	rec2 := httptest.NewRecorder()
	wh.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("duplicate delivery: expected 200, got %d", rec2.Code)
	}
	if !dedupe.SeenOrRecord("evt_dup") {
		t.Fatalf("expected deduper to have recorded evt_dup")
	}
}

func TestWebhook_RejectsOversizedBody(t *testing.T) {
	wh, _ := newTestWebhook(t, nil)
	// Past maxBodyBytes (262144). The handler truncates the read; the
	// resulting body won't verify against the signature.
	body := []byte(`{"id":"evt_big","type":"customer.created","data":{"object":` +
		strings.Repeat(`"x"`, 300_000) + `}}`)
	sig := stripeSignature(t, body, testWebhookSecret, time.Now())
	req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("expected 4xx on oversized body, got %d", rec.Code)
	}
}

// TestHandleSetupIntentFailed_PropagatesToPaymentMethod verifies the
// declined-card fix: when Stripe sends setup_intent.setup_failed, the
// parent PaymentMethod must move to Failed with the failure reason +
// message lifted off last_setup_error, plus an InstrumentReady=False
// condition. Without this propagation the parent PM sat in
// AwaitingConfirmation forever even though the SPM was marked Failed,
// leaving the portal with a broken-looking card the user couldn't
// understand.
func TestHandleSetupIntentFailed_PropagatesToPaymentMethod(t *testing.T) {
	ns := "default"
	pmName := "pm-declined"
	pm := &billingv1alpha1.PaymentMethod{
		ObjectMeta: metav1.ObjectMeta{Name: pmName, Namespace: ns},
		Spec: billingv1alpha1.PaymentMethodSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "ba-declined"},
			DisplayName:       "Declined card",
			PaymentMethodClassRef: &billingv1alpha1.PaymentMethodClassRef{Name: "stripe-default"},
		},
		Status: billingv1alpha1.PaymentMethodStatus{
			Phase: billingv1alpha1.PaymentMethodPhaseAwaitingConfirmation,
		},
	}
	spm := &stripev1alpha1.StripePaymentMethod{
		ObjectMeta: metav1.ObjectMeta{Name: pmName, Namespace: ns},
		Spec: stripev1alpha1.StripePaymentMethodSpec{
			PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pmName},
		},
		Status: stripev1alpha1.StripePaymentMethodStatus{
			Phase: stripev1alpha1.StripePaymentMethodPhaseAwaitingConfirmation,
			SetupIntent: &stripev1alpha1.StripeSetupIntentStatus{
				ID:     "seti_declined_test",
				Status: "requires_payment_method",
			},
		},
	}
	wh, c := newTestWebhook(t, nil, pm, spm)
	// Status subresource isn't writable through the fake builder
	// (it's set above via the typed Status field on Build), so this
	// is a no-op as long as the WithStatusSubresource registration
	// in newTestWebhook lined the types up. Pre-create body verified.

	body := []byte(`{
		"id":"evt_failed",
		"type":"setup_intent.setup_failed",
		"data":{"object":{
			"id":"seti_declined_test",
			"status":"requires_payment_method",
			"last_setup_error":{
				"code":"card_declined",
				"message":"Your card was declined."
			},
			"metadata":{
				"stripe_payment_method_namespace":"default",
				"stripe_payment_method_name":"pm-declined"
			}
		}}
	}`)
	sig := stripeSignature(t, body, testWebhookSecret, time.Now())
	req := httptest.NewRequest(http.MethodPost, Endpoint, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from successful failure-event handling, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Re-read the parent PaymentMethod and assert it's flipped to
	// Failed with the decline reason / message propagated.
	var got billingv1alpha1.PaymentMethod
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: pmName}, &got); err != nil {
		t.Fatalf("get parent PaymentMethod: %v", err)
	}
	if got.Status.Phase != billingv1alpha1.PaymentMethodPhaseFailed {
		t.Errorf("PaymentMethod.Status.Phase: want Failed, got %q", got.Status.Phase)
	}
	if got.Status.FailureReason != "card_declined" {
		t.Errorf("PaymentMethod.Status.FailureReason: want card_declined, got %q", got.Status.FailureReason)
	}
	if got.Status.FailureMessage != "Your card was declined." {
		t.Errorf("PaymentMethod.Status.FailureMessage: want decline message, got %q", got.Status.FailureMessage)
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == billingv1alpha1.PaymentMethodConditionInstrumentReady {
			ready = &got.Status.Conditions[i]
			break
		}
	}
	if ready == nil {
		t.Fatalf("expected InstrumentReady condition on PaymentMethod")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("InstrumentReady condition: want False, got %s", ready.Status)
	}
	if ready.Reason != "card_declined" {
		t.Errorf("InstrumentReady reason: want card_declined, got %q", ready.Reason)
	}
}
