// SPDX-License-Identifier: AGPL-3.0-only

// Package stripe wraps the Stripe SDK and exposes only the operations the
// provider controllers need (Customer + SetupIntent + PaymentMethod
// retrieval + webhook signature verification).
package stripe

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// ResolvedConfig holds materialised values from a StripeProviderConfig +
// its referenced Secrets, ready for use by the SDK client and webhook
// verifier.
type ResolvedConfig struct {
	ProviderConfigName string
	PublishableKey     string
	SecretKey          string
	WebhookSecret      string
	APIVersion         string
}

// SecretNamespace is the namespace from which the Stripe provider
// dereferences SecretKeySelector references. Pinned to keep the trust
// boundary explicit.
const SecretNamespace = "stripe-provider-system"

// ResolveConfig loads a StripeProviderConfig by name and dereferences its
// Secret references.
func ResolveConfig(ctx context.Context, c client.Reader, name string) (*ResolvedConfig, error) {
	var cfg stripev1alpha1.StripeProviderConfig
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &cfg); err != nil {
		return nil, fmt.Errorf("getting StripeProviderConfig %q: %w", name, err)
	}
	sec, err := readSecretKey(ctx, c, cfg.Spec.SecretKeyRef)
	if err != nil {
		return nil, fmt.Errorf("reading secretKey: %w", err)
	}
	wh, err := readSecretKey(ctx, c, cfg.Spec.WebhookSecretRef)
	if err != nil {
		return nil, fmt.Errorf("reading webhookSecret: %w", err)
	}
	return &ResolvedConfig{
		ProviderConfigName: cfg.Name,
		PublishableKey:     cfg.Spec.PublishableKey,
		SecretKey:          sec,
		WebhookSecret:      wh,
		APIVersion:         cfg.Spec.APIVersion,
	}, nil
}

func readSecretKey(ctx context.Context, c client.Reader, ref corev1.SecretKeySelector) (string, error) {
	if ref.Name == "" || ref.Key == "" {
		return "", errors.New("SecretKeySelector must set both name and key")
	}
	var s corev1.Secret
	key := types.NamespacedName{Namespace: SecretNamespace, Name: ref.Name}
	if err := c.Get(ctx, key, &s); err != nil {
		return "", fmt.Errorf("getting Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	v, ok := s.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", key.Namespace, key.Name, ref.Key)
	}
	return string(v), nil
}
