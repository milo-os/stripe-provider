// SPDX-License-Identifier: AGPL-3.0-only

// Package webhook hosts the Stripe webhook receiver and exposes it as
// a controller-runtime webhook.Server-registered handler. The receiver
// resolves a StripeProviderConfig per request, verifies the Stripe
// signature, dedupes on event id, and applies side effects to
// StripePaymentMethod / PaymentMethod via the controller-runtime
// client.
//
// The Webhook / Handler / HandlerFunc / Request / Response abstraction
// mirrors the pattern used by sibling provider repos
// (milo-os/loops-provider, milo-os/zitadel-provider,
// milo-os/openfga-provider) so the wire shape and operational tooling
// (metrics, structured logs) are consistent across providers.
package webhook

import (
	"context"

	stripego "github.com/stripe/stripe-go/v81"
)

// Endpoint is the route Stripe is configured to deliver events to.
const Endpoint = "/webhooks/stripe"

// Request carries the decoded Stripe event delivered to the webhook.
type Request struct {
	// Event is the full Stripe event envelope.
	Event *stripego.Event
}

// Response is the outcome of handling a webhook request.
type Response struct {
	// HttpStatus is the status code to write back to Stripe.
	HttpStatus int
}

// Handler processes a Stripe webhook event.
type Handler interface {
	Handle(context.Context, Request) Response
}

// HandlerFunc adapts an ordinary function to the Handler interface.
type HandlerFunc func(context.Context, Request) Response

// Handle invokes the underlying function.
func (f HandlerFunc) Handle(ctx context.Context, req Request) Response {
	return f(ctx, req)
}

var _ Handler = HandlerFunc(nil)
