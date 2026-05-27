// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const outcomeLabel = "outcome"
const eventTypeLabel = "event_type"

// Outcome label values for webhook metrics.
const (
	OutcomeSuccess           = "success"
	OutcomeDuplicate         = "duplicate"
	OutcomeInvalidSignature  = "invalid_signature"
	OutcomeMissingSignature  = "missing_signature"
	OutcomeMalformed         = "malformed"
	OutcomeMethodNotAllowed  = "method_not_allowed"
	OutcomeProviderConfigErr = "provider_config_error"
	OutcomeHandlerError      = "handler_error"
	OutcomeIgnored           = "ignored"
)

var (
	// stripeWebhookRequestsTotal counts incoming Stripe webhook
	// requests by event type and outcome. Use this to track delivery
	// volume and surface failure rates.
	stripeWebhookRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "stripe_webhook_requests_total",
			Help: "Total Stripe webhook requests received by event type and outcome.",
		},
		[]string{eventTypeLabel, outcomeLabel},
	)

	// stripeWebhookRequestDuration measures the end-to-end duration of
	// Stripe webhook handling, labeled by event type and outcome.
	stripeWebhookRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "stripe_webhook_request_duration_seconds",
			Help:    "Duration of Stripe webhook request processing in seconds.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		},
		[]string{eventTypeLabel, outcomeLabel},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		stripeWebhookRequestsTotal,
		stripeWebhookRequestDuration,
	)
}
