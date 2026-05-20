# stripe-provider

Stripe reference implementation of the payment provider pattern for the
[milo-os/billing](https://github.com/milo-os/billing) `PaymentMethod` and
`PaymentMethodClass` CRDs.

The billing service intentionally knows nothing about Stripe; it only owns
the generic `PaymentMethod` resource and the cluster-scoped
`PaymentMethodClass` registry. This service watches `PaymentMethod`
resources that name a `PaymentMethodClass` belonging to it, drives the
Stripe SetupIntent lifecycle through a provider-owned `StripePaymentMethod`
CRD, and projects normalized outcome state back onto `PaymentMethod`
status once the instrument is confirmed.

See [`docs/enhancements/payment-methods.md`](https://github.com/milo-os/billing/blob/main/docs/enhancements/payment-methods.md)
in the billing repo for the full design.

## CRDs

| Resource | Scope | Purpose |
|---|---|---|
| `StripeProviderConfig` | Cluster | Operator-configured Stripe SDK settings (publishable key) referenced by a `PaymentMethodClass.spec.parametersRef`. |
| `StripePaymentMethod` | Namespace | Provider-owned state for a single `PaymentMethod`. Child of the parent `PaymentMethod` via `ownerReference`. |

## Components

- **PaymentMethod watcher** — watches billing `PaymentMethod` resources;
  creates a `StripePaymentMethod` child when the resource's class points
  at a Stripe `PaymentMethodClass`.
- **StripePaymentMethod controller** — ensures a Stripe `Customer` for
  the billing account, creates a `SetupIntent`, writes `clientSecret`
  onto `StripePaymentMethod.status.setupIntent`, then projects normalized
  details onto `PaymentMethod.status` once confirmation arrives.
- **Webhook server** — receives Stripe `setup_intent.*` events on
  `POST /webhooks/stripe`. Signature-verified via the per-class webhook
  secret stored in the `Secret` referenced by `StripeProviderConfig`.

## Develop

```
task install-tools
task generate
task manifests
task build
task test
task lint
```
