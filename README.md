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
| `StripeCustomer` | Namespace | The Stripe `Customer` for a `BillingAccount`, named after it and owned by it. `status.stripeCustomerId` is what invoicing providers bill, so an account with payment terms can be invoiced without a card. |

## Components

- **PaymentMethod watcher** — watches billing `PaymentMethod` resources;
  creates a `StripePaymentMethod` child when the resource's class points
  at a Stripe `PaymentMethodClass`.
- **StripePaymentMethod controller** — ensures a Stripe `Customer` for
  the billing account, creates a `SetupIntent`, writes `clientSecret`
  onto `StripePaymentMethod.status.setupIntent`, then projects normalized
  details onto `PaymentMethod.status` once confirmation arrives.
- **BillingAccount controller** — keeps the account's Stripe `Customer`
  in sync with the `BillingAccount`, and creates it, with its
  `StripeCustomer`, once the account has a payment method or billing
  reports it is billed by invoice (`status.paymentArrangement`). Both
  controllers create customers through one shared, per-account lock, so
  an account never gets two.
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
