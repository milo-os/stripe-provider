// SPDX-License-Identifier: AGPL-3.0-only

// Package stripe wraps the Stripe SDK and exposes only the operations the
// provider controllers need (Customer + SetupIntent + PaymentMethod
// retrieval + webhook signature verification).
package stripe

import (
	"context"
	"errors"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// ResolvedConfig holds materialised values from a StripeProviderConfig +
// the controller-pod environment, ready for use by the SDK client and
// webhook verifier.
type ResolvedConfig struct {
	ProviderConfigName string
	PublishableKey     string
	SecretKey          string
	WebhookSecret      string
	APIVersion         string
}

const (
	// SecretKeyEnv is the environment variable the controller reads to
	// obtain the Stripe secret API key. Deployment operators source it
	// from a Kubernetes Secret (typically backed by an external secret
	// manager) so the credential never appears on a user-facing API
	// resource.
	SecretKeyEnv = "STRIPE_SECRET_KEY"

	// WebhookSecretEnv is the environment variable the controller reads
	// to obtain the Stripe webhook signing secret. Same sourcing model
	// as SecretKeyEnv.
	WebhookSecretEnv = "STRIPE_WEBHOOK_SECRET"
)

// ResolveConfig loads a StripeProviderConfig by name for its non-sensitive
// fields and pulls the SDK credentials from the controller-pod
// environment.
func ResolveConfig(ctx context.Context, c client.Reader, name string) (*ResolvedConfig, error) {
	var cfg stripev1alpha1.StripeProviderConfig
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &cfg); err != nil {
		return nil, fmt.Errorf("getting StripeProviderConfig %q: %w", name, err)
	}
	sec := os.Getenv(SecretKeyEnv)
	if sec == "" {
		return nil, fmt.Errorf("%s environment variable is empty: Stripe SDK calls cannot be authenticated", SecretKeyEnv)
	}
	wh := os.Getenv(WebhookSecretEnv)
	if wh == "" {
		return nil, fmt.Errorf("%s environment variable is empty: incoming Stripe webhooks cannot be verified", WebhookSecretEnv)
	}
	if cfg.Spec.PublishableKey == "" {
		return nil, errors.New("StripeProviderConfig.spec.publishableKey is empty")
	}
	return &ResolvedConfig{
		ProviderConfigName: cfg.Name,
		PublishableKey:     cfg.Spec.PublishableKey,
		SecretKey:          sec,
		WebhookSecret:      wh,
		APIVersion:         cfg.Spec.APIVersion,
	}, nil
}
