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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

const (
	testWebhookSecret = "whsec_test_dummy_secret_value_xx"
	testProviderName  = "test-stripe"
)

// stripeSignature builds a Stripe-compatible Stripe-Signature header for
// the given body + timestamp using the same HMAC-SHA256 scheme the
// stripe-go webhook verifier expects.
func stripeSignature(t *testing.T, body []byte, secret string, ts time.Time) string {
	t.Helper()
	tsStr := fmt.Sprintf("%d", ts.Unix())
	payload := tsStr + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return "t=" + tsStr + ",v1=" + sig
}

func newTestHandler(t *testing.T, deduper *stripeinternal.MemoryDeduper, extra ...client.Object) (*handler, client.Client) {
	t.Helper()
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
	cfg.Name = testProviderName
	cfg.Spec.PublishableKey = "pk_test_dummy"
	cfg.Spec.SecretKeyRef = corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "stripe-keys"},
		Key:                  "secret",
	}
	cfg.Spec.WebhookSecretRef = corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "stripe-keys"},
		Key:                  "webhook",
	}
	secret := &corev1.Secret{}
	secret.Name = "stripe-keys"
	secret.Namespace = stripeinternal.SecretNamespace
	secret.Data = map[string][]byte{
		"secret":  []byte("sk_test_dummy"),
		"webhook": []byte(testWebhookSecret),
	}

	objects := []client.Object{cfg, secret}
	objects = append(objects, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(
		&stripev1alpha1.StripePaymentMethod{},
		&billingv1alpha1.PaymentMethod{},
	).Build()

	if deduper == nil {
		deduper = stripeinternal.NewMemoryDeduper(0)
	}
	return &handler{
		client:             c,
		providerConfigName: testProviderName,
		dedupe:             deduper,
	}, c
}

func TestWebhook_RejectsMissingSignature(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	body := []byte(`{"id":"evt_1","type":"setup_intent.succeeded","data":{}}`)
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on missing signature, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestWebhook_RejectsInvalidSignature(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	body := []byte(`{"id":"evt_1","type":"setup_intent.succeeded","data":{"object":{"id":"seti_1"}}}`)
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid signature, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestWebhook_RejectsNonPost(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET, got %d", rec.Code)
	}
}

func TestWebhook_AcceptsValidSignedUnhandledEventNoOp(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	body := []byte(`{"id":"evt_ignored","type":"customer.created","data":{"object":{"id":"cus_1"}}}`)
	sig := stripeSignature(t, body, testWebhookSecret, time.Now())
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unhandled event with valid signature, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestWebhook_DedupesRepeatedEventID(t *testing.T) {
	deduper := stripeinternal.NewMemoryDeduper(0)
	h, _ := newTestHandler(t, deduper)
	body := []byte(`{"id":"evt_dup","type":"customer.created","data":{"object":{"id":"cus_1"}}}`)
	sig := stripeSignature(t, body, testWebhookSecret, time.Now())

	// First delivery: processed.
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first delivery: expected 200, got %d", rec.Code)
	}

	// Second delivery with the same event id: also 200, but dropped by the deduper.
	req2 := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req2.Header.Set("Stripe-Signature", sig)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("duplicate delivery: expected 200, got %d", rec2.Code)
	}
	// Deduper should report the id as seen.
	if !deduper.SeenOrRecord("evt_dup") {
		t.Fatalf("expected deduper to have recorded evt_dup")
	}
}

func TestWebhook_RejectsOversizedBody(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	// Body well past maxBodyBytes (1<<18 = 262144). The handler stops
	// reading at the cap, leaving a truncated body that won't verify.
	body := []byte(`{"id":"evt_big","type":"customer.created","data":{"object":` +
		strings.Repeat(`"x"`, 300_000) + `}}`)
	sig := stripeSignature(t, body, testWebhookSecret, time.Now())
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Truncated body fails signature verification rather than producing a
	// 200; we just assert it doesn't crash and returns a 4xx.
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("expected 4xx on oversized body, got %d", rec.Code)
	}
}
