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
