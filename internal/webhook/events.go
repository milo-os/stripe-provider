// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"maps"
	"time"

	"k8s.io/client-go/tools/record"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// eventRecorderName is the component the activity timeline shows as the
// actor of stripe-provider's Events.
const eventRecorderName = "stripe-provider"

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// recordPaymentMethodEvent records an activity timeline Event on pm with the
// given detail annotations, scoped to pm's organization so its members see
// it. The reasons and annotation keys are billing's (see
// api/v1alpha1/events.go in go.miloapis.com/billing). A nil recorder records
// nothing.
func recordPaymentMethodEvent(
	recorder record.EventRecorder,
	pm *billingv1alpha1.PaymentMethod,
	detail map[string]string,
	eventType, reason, message string,
) {
	if recorder == nil {
		return
	}
	annotations := map[string]string{}
	maps.Copy(annotations, billingv1alpha1.OrganizationScopeAnnotations(pm.Namespace))
	maps.Copy(annotations, detail)
	recorder.AnnotatedEventf(pm, annotations, eventType, reason, "%s", message)
}

// attachedEventDetail describes the confirmed payment method: its billing
// account and, for a card, its brand, last four digits and expiry.
func attachedEventDetail(pm *billingv1alpha1.PaymentMethod) map[string]string {
	detail := map[string]string{
		billingv1alpha1.EventAnnotationBillingAccount: pm.Spec.BillingAccountRef.Name,
	}
	if pm.Status.Details == nil || pm.Status.Details.Card == nil {
		return detail
	}
	card := pm.Status.Details.Card
	detail[billingv1alpha1.EventAnnotationCardBrand] = card.Brand
	detail[billingv1alpha1.EventAnnotationCardLast4] = card.Last4
	if card.ExpiryYear > 0 && card.ExpiryMonth >= 1 && card.ExpiryMonth <= 12 {
		expiry := time.Date(int(card.ExpiryYear), time.Month(card.ExpiryMonth), 1, 0, 0, 0, 0, time.UTC)
		detail[billingv1alpha1.EventAnnotationCardExpiry] = billingv1alpha1.EventTime(expiry)
	}
	return detail
}

// failedEventDetail describes a failed setup: the billing account and, when
// Stripe said why, the normalized decline reason.
func failedEventDetail(pm *billingv1alpha1.PaymentMethod, si *setupIntentPayload) map[string]string {
	detail := map[string]string{
		billingv1alpha1.EventAnnotationBillingAccount: pm.Spec.BillingAccountRef.Name,
	}
	if si.LastSetupError != nil {
		detail[billingv1alpha1.EventAnnotationDeclineReason] = string(setupDeclineReason(si.LastSetupError.Code, si.LastSetupError.DeclineCode))
	}
	return detail
}

// setupDeclineReason maps the error Stripe gave for a failed SetupIntent
// onto billing's normalized DeclineReason. Stripe's codes never reach the
// timeline.
func setupDeclineReason(code, declineCode string) billingv1alpha1.DeclineReason {
	is := func(codes ...string) bool {
		for _, c := range codes {
			if code == c || declineCode == c {
				return true
			}
		}
		return false
	}
	switch {
	case is("insufficient_funds"):
		return billingv1alpha1.DeclineReasonInsufficientFunds
	case is("expired_card"):
		return billingv1alpha1.DeclineReasonExpiredCard
	case is("authentication_required", "setup_intent_authentication_failure"):
		return billingv1alpha1.DeclineReasonAuthenticationRequired
	case is("processing_error", "issuer_not_available", "try_again_later", "reenter_transaction"):
		return billingv1alpha1.DeclineReasonProcessingError
	}
	return billingv1alpha1.DeclineReasonDeclined
}
