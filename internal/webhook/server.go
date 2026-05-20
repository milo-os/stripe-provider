// SPDX-License-Identifier: AGPL-3.0-only

// Package webhook hosts the public Stripe webhook receiver. The receiver
// resolves a StripeProviderConfig per request, verifies the Stripe
// signature, dedupes on event id, and applies side effects to
// StripePaymentMethod / PaymentMethod via the controller-runtime client.
package webhook

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	stripego "github.com/stripe/stripe-go/v81"
	stripewebhook "github.com/stripe/stripe-go/v81/webhook"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

const (
	// Path is the public route Stripe is configured to deliver events to.
	Path = "/webhooks/stripe"

	maxBodyBytes       = 1 << 18
	signatureTolerance = 5 * time.Minute
	shutdownTimeout    = 10 * time.Second
)

var log = ctrl.Log.WithName("stripe-webhook")

// ServerOptions configures the standalone HTTP listener that receives
// Stripe webhooks.
type ServerOptions struct {
	// Addr is the listen address (e.g. ":8090").
	Addr string

	// ProviderConfigName names the StripeProviderConfig used to verify
	// signatures and look up the secret key for follow-up Stripe API
	// calls.
	ProviderConfigName string

	// TLSCertFile / TLSKeyFile enable in-process TLS termination. In
	// typical deployments TLS is terminated upstream and these are left
	// empty.
	TLSCertFile string
	TLSKeyFile  string
}

// NewRunnable builds a manager.Runnable that serves the webhook.
func NewRunnable(opts ServerOptions, mgr manager.Manager) (manager.Runnable, error) {
	if opts.Addr == "" {
		return nil, errors.New("stripe webhook server: Addr is required")
	}
	if opts.ProviderConfigName == "" {
		return nil, errors.New("stripe webhook server: ProviderConfigName is required")
	}

	handler := &handler{
		client:             mgr.GetClient(),
		providerConfigName: opts.ProviderConfigName,
		dedupe:             stripeinternal.NewMemoryDeduper(0),
	}

	mux := http.NewServeMux()
	mux.Handle(Path, handler)

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading TLS cert: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}
	return &runnable{srv: srv}, nil
}

type runnable struct {
	srv *http.Server
}

func (r *runnable) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		log.Info("starting Stripe webhook server", "addr", r.srv.Addr, "tls", r.srv.TLSConfig != nil)
		var err error
		if r.srv.TLSConfig != nil {
			err = r.srv.ListenAndServeTLS("", "")
		} else {
			err = r.srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = r.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

type handler struct {
	client             client.Client
	providerConfigName string
	dedupe             *stripeinternal.MemoryDeduper
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sig := r.Header.Get("Stripe-Signature")
	if sig == "" {
		http.Error(w, "missing Stripe-Signature", http.StatusBadRequest)
		return
	}

	cfg, err := stripeinternal.ResolveConfig(ctx, h.client, h.providerConfigName)
	if err != nil {
		log.Error(err, "resolving StripeProviderConfig", "name", h.providerConfigName)
		// 500 so Stripe retries.
		http.Error(w, "provider unavailable", http.StatusInternalServerError)
		return
	}

	event, err := stripewebhook.ConstructEventWithOptions(body, sig, cfg.WebhookSecret, stripewebhook.ConstructEventOptions{
		Tolerance: signatureTolerance,
		// Stripe accounts pin an API version in the dashboard; the
		// SDK's pinned version moves more slowly. Mismatches here are
		// not actionable — the event still decodes correctly — so we
		// tolerate them rather than reject otherwise-valid webhooks
		// after SDK upgrades.
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		log.Info("rejecting webhook with invalid signature", "err", err.Error())
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	if h.dedupe != nil && h.dedupe.SeenOrRecord(event.ID) {
		log.V(1).Info("dropping duplicate event", "id", event.ID, "type", event.Type)
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := h.dispatch(ctx, cfg, &event); err != nil {
		log.Error(err, "dispatching event", "id", event.ID, "type", event.Type)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handler) dispatch(ctx context.Context, cfg *stripeinternal.ResolvedConfig, event *stripego.Event) error {
	switch event.Type {
	case "setup_intent.succeeded":
		return h.handleSetupIntentSucceeded(ctx, cfg, event)
	case "setup_intent.setup_failed", "setup_intent.canceled":
		return h.handleSetupIntentFailed(ctx, event)
	default:
		log.V(1).Info("ignoring unhandled event", "type", event.Type, "id", event.ID)
		return nil
	}
}

// setupIntentPayload is the subset of the SetupIntent JSON we care about.
type setupIntentPayload struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Customer       string `json:"customer"`
	PaymentMethod  string `json:"payment_method"`
	LastSetupError *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"last_setup_error"`
	Metadata map[string]string `json:"metadata"`
}

func decodeSetupIntent(event *stripego.Event) (*setupIntentPayload, error) {
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return nil, errors.New("event has no data")
	}
	var si setupIntentPayload
	if err := json.Unmarshal(event.Data.Raw, &si); err != nil {
		return nil, fmt.Errorf("decoding SetupIntent payload: %w", err)
	}
	if si.ID == "" {
		return nil, errors.New("SetupIntent payload missing id")
	}
	return &si, nil
}

func (h *handler) handleSetupIntentSucceeded(ctx context.Context, cfg *stripeinternal.ResolvedConfig, event *stripego.Event) error {
	si, err := decodeSetupIntent(event)
	if err != nil {
		return err
	}
	spm, err := h.findStripePaymentMethod(ctx, si)
	if err != nil || spm == nil {
		return err
	}

	stripe := stripeinternal.NewClient(cfg)
	pmDetails, err := stripe.RetrievePaymentMethod(ctx, si.PaymentMethod)
	if err != nil {
		return fmt.Errorf("retrieving PaymentMethod for SetupIntent %q: %w", si.ID, err)
	}

	if err := h.patchStripeSuccess(ctx, spm, si, pmDetails); err != nil {
		return err
	}
	return h.projectOntoPaymentMethod(ctx, spm, pmDetails)
}

func (h *handler) handleSetupIntentFailed(ctx context.Context, event *stripego.Event) error {
	si, err := decodeSetupIntent(event)
	if err != nil {
		return err
	}
	spm, err := h.findStripePaymentMethod(ctx, si)
	if err != nil || spm == nil {
		return err
	}
	return h.patchStripeFailure(ctx, spm, si)
}

// findStripePaymentMethod locates the StripePaymentMethod that owns the
// supplied SetupIntent. Prefers the metadata pointer set at creation
// time; falls back to a list/scan keyed on status.setupIntent.id.
func (h *handler) findStripePaymentMethod(ctx context.Context, si *setupIntentPayload) (*stripev1alpha1.StripePaymentMethod, error) {
	if ns, name := si.Metadata["stripe_payment_method_namespace"], si.Metadata["stripe_payment_method_name"]; ns != "" && name != "" {
		var spm stripev1alpha1.StripePaymentMethod
		if err := h.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &spm); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("getting StripePaymentMethod %s/%s: %w", ns, name, err)
		}
		return &spm, nil
	}
	var list stripev1alpha1.StripePaymentMethodList
	if err := h.client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing StripePaymentMethods: %w", err)
	}
	for i := range list.Items {
		spm := &list.Items[i]
		if spm.Status.SetupIntent != nil && spm.Status.SetupIntent.ID == si.ID {
			return spm, nil
		}
	}
	return nil, nil
}

func (h *handler) patchStripeSuccess(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod, si *setupIntentPayload, pm *stripeinternal.PaymentMethodDetails) error {
	now := metav1.Now()
	patch := client.MergeFrom(spm.DeepCopy())
	spm.Status.Phase = stripev1alpha1.StripePaymentMethodPhaseActive
	spm.Status.StripePaymentMethodID = pm.ID
	spm.Status.ConfirmedAt = &now
	if spm.Status.SetupIntent == nil {
		spm.Status.SetupIntent = &stripev1alpha1.StripeSetupIntentStatus{}
	}
	spm.Status.SetupIntent.ID = si.ID
	spm.Status.SetupIntent.Status = si.Status
	spm.Status.SetupIntent.ClientSecret = "" // single-use; clear once consumed.
	if pm.Type == "card" {
		spm.Status.Instrument = &stripev1alpha1.StripeInstrumentDetails{
			Type: stripev1alpha1.StripePaymentMethodInstrumentTypeCard,
			Card: &stripev1alpha1.StripeCardInstrumentDetails{
				Brand:                      pm.Brand,
				Last4:                      pm.Last4,
				IssuerIdentificationNumber: pm.BIN,
				Country:                    pm.Country,
				ExpiryMonth:                pm.ExpMonth,
				ExpiryYear:                 pm.ExpYear,
				AVSResult:                  pm.AVSResult,
				CVCResult:                  pm.CVCResult,
			},
		}
	}
	apimeta.SetStatusCondition(&spm.Status.Conditions, metav1.Condition{
		Type:               "Confirmed",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: spm.Generation,
		Reason:             "SetupIntentSucceeded",
		Message:            fmt.Sprintf("Stripe SetupIntent %s succeeded.", si.ID),
	})
	return h.client.Status().Patch(ctx, spm, patch)
}

func (h *handler) patchStripeFailure(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod, si *setupIntentPayload) error {
	patch := client.MergeFrom(spm.DeepCopy())
	spm.Status.Phase = stripev1alpha1.StripePaymentMethodPhaseFailed
	if spm.Status.SetupIntent == nil {
		spm.Status.SetupIntent = &stripev1alpha1.StripeSetupIntentStatus{}
	}
	spm.Status.SetupIntent.ID = si.ID
	spm.Status.SetupIntent.Status = si.Status
	reason := "SetupIntentFailed"
	msg := fmt.Sprintf("Stripe SetupIntent %s failed.", si.ID)
	if si.LastSetupError != nil {
		spm.Status.FailureReason = si.LastSetupError.Code
		spm.Status.FailureMessage = si.LastSetupError.Message
		if si.LastSetupError.Message != "" {
			msg = si.LastSetupError.Message
		}
	}
	apimeta.SetStatusCondition(&spm.Status.Conditions, metav1.Condition{
		Type:               "Confirmed",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: spm.Generation,
		Reason:             reason,
		Message:            msg,
	})
	return h.client.Status().Patch(ctx, spm, patch)
}

func (h *handler) projectOntoPaymentMethod(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod, pm *stripeinternal.PaymentMethodDetails) error {
	var bm billingv1alpha1.PaymentMethod
	key := types.NamespacedName{Namespace: spm.Namespace, Name: spm.Spec.PaymentMethodRef.Name}
	if err := h.client.Get(ctx, key, &bm); err != nil {
		return fmt.Errorf("getting PaymentMethod %s/%s: %w", key.Namespace, key.Name, err)
	}
	patch := client.MergeFrom(bm.DeepCopy())
	bm.Status.Phase = billingv1alpha1.PaymentMethodPhaseActive
	if pm.Type == "card" {
		bm.Status.Details = &billingv1alpha1.PaymentMethodDetails{
			Type: billingv1alpha1.PaymentMethodInstrumentTypeCard,
			Card: &billingv1alpha1.PaymentMethodCardDetails{
				Brand:                      pm.Brand,
				Last4:                      pm.Last4,
				IssuerIdentificationNumber: pm.BIN,
				Country:                    pm.Country,
				ExpiryMonth:                pm.ExpMonth,
				ExpiryYear:                 pm.ExpYear,
				AVSResult:                  pm.AVSResult,
				CVCResult:                  pm.CVCResult,
			},
		}
	}
	apimeta.SetStatusCondition(&bm.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.PaymentMethodConditionInstrumentReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: bm.Generation,
		Reason:             "Active",
		Message:            "Payment method confirmed by stripe-provider.",
	})
	return h.client.Status().Patch(ctx, &bm, patch)
}
