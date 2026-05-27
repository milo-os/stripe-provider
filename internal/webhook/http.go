// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
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

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

const (
	maxBodyBytes       = 1 << 18
	signatureTolerance = 5 * time.Minute
)

var log = ctrl.Log.WithName("stripe-webhook")

// EventDeduper records and checks Stripe event ids for at-most-once
// processing semantics.
type EventDeduper interface {
	SeenOrRecord(eventID string) bool
}

// Webhook is the registered HTTP handler. The Handler field carries the
// business logic invoked after signature verification + dedupe. In
// production both are wired up by NewStripeWebhook; tests construct a
// Webhook directly with a fake client / deduper / handler.
type Webhook struct {
	// Client is used to resolve the StripeProviderConfig (for
	// signature verification + downstream Stripe API calls) and to
	// patch StripePaymentMethod / PaymentMethod.
	Client client.Client

	// ProviderConfigName names the StripeProviderConfig this webhook
	// authenticates against. The webhook resolves the config + its
	// referenced Secrets per request so secret rotation requires no
	// restart.
	ProviderConfigName string

	// Dedupe drops repeated event ids. Persistence across restarts is
	// unnecessary because the downstream patches are idempotent; the
	// deduper just saves wasted reconciles.
	Dedupe EventDeduper

	// Handler runs after the request is validated. It returns the HTTP
	// status to write back to Stripe.
	Handler Handler
}

// NewStripeWebhook wires the standard Stripe webhook: signature
// verification + dedupe + dispatch on setup_intent.* events.
func NewStripeWebhook(c client.Client, providerConfigName string) *Webhook {
	wh := &Webhook{
		Client:             c,
		ProviderConfigName: providerConfigName,
		Dedupe:             stripeinternal.NewMemoryDeduper(0),
	}
	wh.Handler = HandlerFunc(func(ctx context.Context, req Request) Response {
		return wh.dispatch(ctx, req.Event)
	})
	return wh
}

// SetupWithManager registers the webhook on the manager's shared
// webhook server. The manager owns TLS termination and lifecycle, so
// we don't need a separate http.Server here.
func (wh *Webhook) SetupWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register(Endpoint, wh)
	return nil
}

// ServeHTTP implements http.Handler.
func (wh *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	eventType := "unknown"
	outcome := OutcomeHandlerError
	defer func() {
		stripeWebhookRequestsTotal.WithLabelValues(eventType, outcome).Inc()
		stripeWebhookRequestDuration.WithLabelValues(eventType, outcome).Observe(time.Since(start).Seconds())
	}()
	defer func() {
		if rec := recover(); rec != nil {
			log.Error(nil, "panic in stripe webhook handler", "panic", rec)
			outcome = OutcomeHandlerError
			wh.writeResponse(w, InternalServerErrorResponse())
		}
	}()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		outcome = OutcomeMethodNotAllowed
		wh.writeResponse(w, MethodNotAllowedResponse())
		return
	}

	ctx := r.Context()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		log.Error(err, "reading webhook body")
		outcome = OutcomeMalformed
		wh.writeResponse(w, BadRequestResponse())
		return
	}
	sig := r.Header.Get("Stripe-Signature")
	if sig == "" {
		outcome = OutcomeMissingSignature
		wh.writeResponse(w, BadRequestResponse())
		return
	}

	cfg, err := stripeinternal.ResolveConfig(ctx, wh.Client, wh.ProviderConfigName)
	if err != nil {
		log.Error(err, "resolving StripeProviderConfig", "name", wh.ProviderConfigName)
		outcome = OutcomeProviderConfigErr
		wh.writeResponse(w, InternalServerErrorResponse())
		return
	}

	event, err := stripewebhook.ConstructEventWithOptions(body, sig, cfg.WebhookSecret, stripewebhook.ConstructEventOptions{
		Tolerance: signatureTolerance,
		// Stripe accounts pin an API version in the dashboard; the
		// SDK's pinned version moves more slowly. Mismatches here
		// aren't actionable — the event still decodes — so we
		// tolerate them rather than reject otherwise-valid webhooks
		// after SDK upgrades.
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		log.Info("rejecting webhook with invalid signature", "err", err.Error())
		outcome = OutcomeInvalidSignature
		wh.writeResponse(w, BadRequestResponse())
		return
	}
	eventType = string(event.Type)

	if wh.Dedupe != nil && wh.Dedupe.SeenOrRecord(event.ID) {
		log.V(1).Info("dropping duplicate event", "id", event.ID, "type", event.Type)
		outcome = OutcomeDuplicate
		wh.writeResponse(w, OkResponse())
		return
	}

	resp := wh.Handler.Handle(ctx, Request{Event: &event})
	switch {
	case resp.HttpStatus >= 200 && resp.HttpStatus < 300:
		outcome = OutcomeSuccess
	case resp.HttpStatus >= 500:
		outcome = OutcomeHandlerError
	default:
		outcome = OutcomeMalformed
	}
	wh.writeResponse(w, resp)
}

func (wh *Webhook) writeResponse(w http.ResponseWriter, resp Response) {
	w.WriteHeader(resp.HttpStatus)
}

// dispatch routes a verified Stripe event to the right handler.
// Unknown event types are explicitly OK'd so Stripe stops retrying.
func (wh *Webhook) dispatch(ctx context.Context, event *stripego.Event) Response {
	switch event.Type {
	case "setup_intent.succeeded":
		if err := wh.handleSetupIntentSucceeded(ctx, event); err != nil {
			log.Error(err, "handling setup_intent.succeeded", "id", event.ID)
			return InternalServerErrorResponse()
		}
		return OkResponse()
	case "setup_intent.setup_failed", "setup_intent.canceled":
		if err := wh.handleSetupIntentFailed(ctx, event); err != nil {
			log.Error(err, "handling setup_intent failure", "id", event.ID, "type", event.Type)
			return InternalServerErrorResponse()
		}
		return OkResponse()
	default:
		log.V(1).Info("ignoring unhandled event", "type", event.Type, "id", event.ID)
		return OkResponse()
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

func (wh *Webhook) handleSetupIntentSucceeded(ctx context.Context, event *stripego.Event) error {
	si, err := decodeSetupIntent(event)
	if err != nil {
		return err
	}
	spm, err := wh.findStripePaymentMethod(ctx, si)
	if err != nil || spm == nil {
		return err
	}

	cfg, err := stripeinternal.ResolveConfig(ctx, wh.Client, wh.ProviderConfigName)
	if err != nil {
		return err
	}
	pmDetails, err := stripeinternal.NewClient(cfg).RetrievePaymentMethod(ctx, si.PaymentMethod)
	if err != nil {
		return fmt.Errorf("retrieving PaymentMethod for SetupIntent %q: %w", si.ID, err)
	}
	if err := wh.patchStripeSuccess(ctx, spm, si, pmDetails); err != nil {
		return err
	}
	return wh.projectOntoPaymentMethod(ctx, spm, pmDetails)
}

func (wh *Webhook) handleSetupIntentFailed(ctx context.Context, event *stripego.Event) error {
	si, err := decodeSetupIntent(event)
	if err != nil {
		return err
	}
	spm, err := wh.findStripePaymentMethod(ctx, si)
	if err != nil || spm == nil {
		return err
	}
	return wh.patchStripeFailure(ctx, spm, si)
}

func (wh *Webhook) findStripePaymentMethod(ctx context.Context, si *setupIntentPayload) (*stripev1alpha1.StripePaymentMethod, error) {
	if ns, name := si.Metadata["stripe_payment_method_namespace"], si.Metadata["stripe_payment_method_name"]; ns != "" && name != "" {
		var spm stripev1alpha1.StripePaymentMethod
		if err := wh.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &spm); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("getting StripePaymentMethod %s/%s: %w", ns, name, err)
		}
		return &spm, nil
	}
	var list stripev1alpha1.StripePaymentMethodList
	if err := wh.Client.List(ctx, &list); err != nil {
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

func (wh *Webhook) patchStripeSuccess(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod, si *setupIntentPayload, pm *stripeinternal.PaymentMethodDetails) error {
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
	return wh.Client.Status().Patch(ctx, spm, patch)
}

func (wh *Webhook) patchStripeFailure(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod, si *setupIntentPayload) error {
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
	return wh.Client.Status().Patch(ctx, spm, patch)
}

func (wh *Webhook) projectOntoPaymentMethod(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod, pm *stripeinternal.PaymentMethodDetails) error {
	var bm billingv1alpha1.PaymentMethod
	key := types.NamespacedName{Namespace: spm.Namespace, Name: spm.Spec.PaymentMethodRef.Name}
	if err := wh.Client.Get(ctx, key, &bm); err != nil {
		return fmt.Errorf("getting PaymentMethod %s/%s: %w", key.Namespace, key.Name, err)
	}
	patch := client.MergeFrom(bm.DeepCopy())
	bm.Status.Phase = billingv1alpha1.PaymentMethodPhaseActive
	if pm.Type == "card" {
		card := &billingv1alpha1.PaymentMethodCardDetails{
			Brand:                      pm.Brand,
			Last4:                      pm.Last4,
			IssuerIdentificationNumber: pm.BIN,
			Country:                    pm.Country,
			ExpiryMonth:                pm.ExpMonth,
			ExpiryYear:                 pm.ExpYear,
			AVSResult:                  pm.AVSResult,
			CVCResult:                  pm.CVCResult,
		}
		if pm.BillingAddress != nil {
			card.BillingAddress = &billingv1alpha1.CardBillingAddress{
				Country:    pm.BillingAddress.Country,
				Line1:      pm.BillingAddress.Line1,
				Line2:      pm.BillingAddress.Line2,
				City:       pm.BillingAddress.City,
				Region:     pm.BillingAddress.State,
				PostalCode: pm.BillingAddress.PostalCode,
			}
		}
		bm.Status.Details = &billingv1alpha1.PaymentMethodDetails{
			Type: billingv1alpha1.PaymentMethodInstrumentTypeCard,
			Card: card,
		}
	}
	apimeta.SetStatusCondition(&bm.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.PaymentMethodConditionInstrumentReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: bm.Generation,
		Reason:             "Active",
		Message:            "Payment method confirmed by stripe-provider.",
	})
	return wh.Client.Status().Patch(ctx, &bm, patch)
}
