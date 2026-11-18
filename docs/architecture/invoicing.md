<!-- omit from toc -->
# Invoicing Architecture

- [Summary](#summary)
- [Where Each Piece Lives](#where-each-piece-lives)
  - [Why stripe-provider writes the invoices](#why-stripe-provider-writes-the-invoices)
- [Resources](#resources)
  - [StripeProduct](#stripeproduct)
  - [StripeInvoice](#stripeinvoice)
- [Controllers](#controllers)
  - [Product controller](#product-controller)
  - [Invoice controller](#invoice-controller)
  - [Preview controller](#preview-controller)
  - [Webhooks](#webhooks)
- [The Invoice Lifecycle](#the-invoice-lifecycle)
- [Writing Lines](#writing-lines)
  - [Products and prices](#products-and-prices)
  - [Service periods](#service-periods)
  - [Tax](#tax)
  - [Discounts](#discounts)
  - [Notes and metadata](#notes-and-metadata)
  - [Checking amounts](#checking-amounts)
- [Previews](#previews)
- [Idempotency and Failure](#idempotency-and-failure)
- [What Other Repos Do](#what-other-repos-do)
  - [billing](#billing)
  - [openmeter-provider](#openmeter-provider)
  - [staff-portal](#staff-portal)
- [Implementation Phases](#implementation-phases)
- [Observability](#observability)
- [Open Questions](#open-questions)
- [References](#references)

## Summary

This is how stripe-provider implements the
[Writing Our Own Stripe Invoices][enhancement] enhancement. OpenMeter keeps
rating usage, scheduling fees and assembling one invoice per billing period.
Instead of OpenMeter's Stripe app sending that invoice to Stripe, we write
it, line by line, with a Stripe Product (SKU and tax code), a quantity, a unit
price, a service period, tax settings, a staff note and our metadata.

stripe-provider gains two resources:

- **`StripeProduct`**: one per SKU, keeping a Stripe Product in step with
  the catalog.
- **`StripeInvoice`**: an invoice to write to Stripe, as lines. The same
  resource previews an invoice without creating one, or creates a preview
  draft for staff to open in the Stripe dashboard.

openmeter-provider turns OpenMeter's draft invoices into `StripeInvoice`s
and reports Stripe's results back to OpenMeter. stripe-provider never calls
OpenMeter.

## Where Each Piece Lives

| Piece | Repo | Today | With this design |
|---|---|---|---|
| Stripe customer, address, tax IDs, payment terms | stripe-provider | `StripeCustomer`, BillingAccount controller | Unchanged |
| Stripe Products | stripe-provider | None | `StripeProduct`, from `ServicePricing`s |
| Writing Stripe invoices and lines | stripe-provider | OpenMeter's Stripe app | `StripeInvoice` |
| Previews, from Stripe, with tax | stripe-provider | Our own arithmetic, in openmeter-provider | `StripeInvoice` with `purpose: Preview` or `PreviewDraft` |
| Invoice payment events | stripe-provider | OpenMeter's Stripe app, and polling in openmeter-provider | Webhooks, onto `StripeInvoice` status |
| Rating, fee schedules, invoice assembly | OpenMeter | OpenMeter | Unchanged |
| Turning OpenMeter invoices into lines | openmeter-provider | OpenMeter's Stripe app | OpenMeter's Custom Invoicing app, and `StripeInvoice` |
| `Invoice` projection for the portals | openmeter-provider | Reads OpenMeter and Stripe | Unchanged |
| Tax codes, SKUs, charges staff add | billing | SKU only | `ServicePricing` tax settings, and a charge resource |

### Why stripe-provider writes the invoices

The enhancement sketched openmeter-provider writing to Stripe. This design
moves the writing here instead:

- **Every Stripe write in one place.** stripe-provider already owns the
  Stripe customer, its address, tax IDs and payment terms. Invoices,
  Products, coupons and previews belong with them, behind the same key,
  client, rate limits and webhook endpoint.
- **Each provider talks to one vendor.** openmeter-provider maps OpenMeter's
  invoices to lines. It doesn't need to know how Stripe wants them written.
  If we move off OpenMeter for any part of billing, the Stripe side is
  unchanged.
- **There's a precedent.** openmeter-provider already reads stripe-provider's
  `StripeCustomer` (`internal/controller/stripe_customer.go`). It would write
  `StripeInvoice`s and read their status the same way.
- **Previews and real invoices share one writer.** Both build Stripe lines
  from the same spec, so a preview can't drift from what's billed.

The cost is one more hop: openmeter-provider waits for `StripeInvoice`
status before it tells OpenMeter a draft is synced. OpenMeter holds the
draft until then, so nothing is lost.

## Resources

Both are in `stripe.miloapis.com/v1alpha1`, next to `StripeCustomer`.

### StripeProduct

One per SKU, in `milo-system`, where `ServicePricing`s live. Named from the
SKU, lower-cased, with a short hash of the exact SKU so case and punctuation
never collide (`sku-dc-cab-01-3f9a2c`). A `ServicePricing` without a SKU
gets one of its own, named `pricing-<name>`.

```yaml
apiVersion: stripe.miloapis.com/v1alpha1
kind: StripeProduct
metadata:
  name: sku-dc-cab-01-3f9a2c
  namespace: milo-system
spec:
  sku: DC-CAB-01              # exact SKU; empty for a pricing without one
  pricingRef: ""              # set only for a pricing without a SKU
status:
  productId: prod_...
  name: Structured cabling    # from the newest list pricing
  taxCode: txcd_...           # from the list pricings
  active: true                # false once no live pricing uses the SKU
  pricings: [cabling-v1, cabling-v2]
  conditions: [Ready, TaxCodeConflict]
```

The product controller creates and deletes these. Nobody else writes them.
Other controllers find a SKU's Product through `status.productId`.

### StripeInvoice

One per invoice to write, in the billing account's namespace, next to its
`StripeCustomer`. The creator owns it. For an OpenMeter invoice that's
openmeter-provider, and the name is derived from OpenMeter's invoice ID.

```yaml
apiVersion: stripe.miloapis.com/v1alpha1
kind: StripeInvoice
metadata:
  name: om-01jd...            # from OpenMeter's invoice ID
  namespace: <account namespace>
spec:
  billingAccountRef: {name: acme}
  purpose: Invoice            # Invoice | Preview | PreviewDraft
  currency: USD
  source:                     # where the invoice came from, for metadata
    system: OpenMeter
    invoiceId: 01JD...
  lines:
    - key: 01JD...LINE        # stable; OpenMeter's line ID for its invoices
      description: Structured cabling
      sku: DC-CAB-01          # resolved to a StripeProduct
      quantity: "4"           # decimal string
      unitAmount: "300.00"    # decimal string, before discounts
      amount: "1200.00"       # what the source charged, before discounts
      period: {start: 2026-12-15T00:00:00Z, end: 2027-01-14T23:59:59Z}
      tax:
        code: txcd_...        # optional; the Product's when empty
        taxable: true
      discountPercent: "10"   # optional
      note: Install delayed a week at the customer's request
      metadata:               # ours, copied onto the Stripe line
        servicePricing: cabling-v2
        listUnitAmount: "350.00"
        billingEntitlement: acme
  finalize: false             # set true when the source issues the invoice
status:
  invoiceId: in_...
  number: ""                  # set at finalization
  state: Draft                # Draft | Open | Paid | Void | Uncollectible | Deleted
  lines:
    - key: 01JD...LINE
      invoiceItemId: ii_...
      amount: "1200.00"
      discountAmount: "120.00"
      taxAmount: "86.40"
  subtotal: "1080.00"
  tax: "86.40"
  total: "1166.40"
  hostedInvoiceUrl: ""        # set at finalization
  dashboardUrl: https://dashboard.stripe.com/invoices/in_...
  conditions: [Synced, Finalized, AmountsMatch]
  observedGeneration: 3
```

Rules, enforced by CEL:

- `purpose` and `billingAccountRef` are immutable.
- Once `finalize` is true, the spec is immutable: Stripe won't change a
  finalized invoice.
- `finalize` is only allowed for `purpose: Invoice`.
- Up to 250 lines, Stripe's limit per invoice.

Customer details come from the account's `StripeCustomer`. Collection method
and days until due come from the `BillingAccount`'s payment arrangement, as
the BillingAccount controller already reads it for the PO number and
agreement reference. They're not repeated on the spec.

## Controllers

### Product controller

Watches `ServicePricing`s in `milo-system`. For each SKU it:

1. creates or updates the `StripeProduct`;
2. creates or updates the Stripe Product: name, `tax_code`, metadata
   (`datum_sku`, the environment) and `active`;
3. sets `TaxCodeConflict` when list pricings with one SKU disagree on their
   tax code, and keeps the Product's current code until they agree.

The rules from the enhancement:

- One Product per SKU, not per price. Customer pricings (`basedOn`) never
  add a Product.
- The Product's name and tax code come from the newest list pricing with the
  SKU.
- No Stripe Prices. Lines pass their unit price inline.
- A Product is archived (`active: false`) once no pricing uses its SKU, and
  reactivated if one does again. Stripe won't delete an invoiced Product.

Products are created when a pricing is applied, so previews can use them
before anything is billed.

### Invoice controller

Reconciles `StripeInvoice`s with `purpose: Invoice`. On each pass:

1. **Wait for dependencies.** The account's `StripeCustomer` is `Ready`, and
   every line's SKU has a `Ready` `StripeProduct`. Otherwise it sets
   `Synced=False` with the reason and requeues.
2. **Create the invoice** if `status.invoiceId` is empty:
   - `customer` from the `StripeCustomer`;
   - `auto_advance: false`, so Stripe never finalizes it on its own;
   - `pending_invoice_items_behavior: exclude`, so no stray items join it;
   - `collection_method` and `days_until_due` from the payment arrangement;
   - `automatic_tax.enabled: true`;
   - metadata: `om_invoice_id` (for OpenMeter's invoices), `datum_stripe_invoice`
     (namespace/name), `datum_purpose`.
   - It records `status.invoiceId` before anything else.
3. **Sync the lines.** For each spec line, create, update or delete the
   invoice item on the invoice, keyed by `datum_line_key` in its metadata. See
   [Writing Lines](#writing-lines). Every item is created with `invoice` set to
   this invoice.
4. **Read back** amounts, discounts and tax per line, and totals, into
   status.
5. **Check amounts.** See [Checking amounts](#checking-amounts). `Synced`
   becomes True only when they match.
6. **Finalize** when `spec.finalize` is true and `Synced` is True: finalize
   the invoice, then record `number`, `hostedInvoiceUrl` and `Finalized=True`.
   Stripe then charges the default payment method or sends the invoice,
   by its collection method.

A `StripeInvoice` deleted before finalization deletes its Stripe draft
(finalizer). One deleted after finalization leaves the Stripe invoice alone:
finalized invoices are voided by staff, never deleted.

### Preview controller

Reconciles `StripeInvoice`s with `purpose: Preview` or `PreviewDraft`. See
[Previews](#previews).

### Webhooks

The webhook server today handles `setup_intent.*` only. It adds:

| Event | Effect |
|---|---|
| `invoice.finalized` | `state: Open`, number and URLs |
| `invoice.paid` | `state: Paid` |
| `invoice.payment_failed` | Condition `PaymentFailed`, with Stripe's reason |
| `invoice.voided` | `state: Void` |
| `invoice.marked_uncollectible` | `state: Uncollectible` |
| `invoice.deleted` | `state: Deleted` |

An event finds its `StripeInvoice` through `datum_stripe_invoice` in the
invoice's metadata. Events for invoices without it (staff-created ones) are
ignored, as today. The invoice controller also refreshes state every few
hours, so a missed event only delays a status.

## The Invoice Lifecycle

For an OpenMeter invoice:

```text
OpenMeter                 openmeter-provider                stripe-provider            Stripe
---------                 ------------------                ---------------            ------
draft, waiting on sync -> builds lines, creates
                          StripeInvoice              ->     creates draft,      ->     draft invoice
                                                            writes lines               with items
                                                     <-     Synced=True
draft synchronized     <- reports Stripe IDs
...collection period...
issuing, waiting       -> sets spec.finalize         ->     finalizes           ->     open; charged
                                                     <-     Finalized=True             or sent
issuing synchronized   <- reports number
                                                     <-     webhooks: Paid ...  <-     invoice.paid
payment status         <- reports payment status
```

If OpenMeter changes a draft before it's issued (late usage, a staff charge
added), openmeter-provider updates the spec, and the invoice controller
updates the lines in place.

## Writing Lines

Each spec line becomes one Stripe invoice item on the invoice.

### Products and prices

- `price_data.product`: the SKU's `StripeProduct`.
- `price_data.unit_amount_decimal`: `unitAmount`, in cents.
- `price_data.tax_behavior`: `exclusive`, always.
- `quantity`: `quantity`. A decimal quantity needs Stripe's
  `quantity_decimal`, which needs a newer API version than stripe-go v81
  gives us. Until then, a line whose quantity isn't whole is written as
  1 × its amount. See [Open Questions](#open-questions).
- `description`: `description`. The SKU is on the Product, so it's no longer
  in the name.

### Service periods

- `period.start` and `period.end` from the line, on every line. Stripe's
  periods are inclusive.
- The source decides the period. stripe-provider only checks that the end
  isn't before the start.
- Revenue Recognition reads them, so a line without a period is refused
  rather than written without one.

### Tax

- The invoice has `automatic_tax.enabled`. Stripe Tax uses the customer's
  address, tax IDs and exemption from the `StripeCustomer`.
- `tax_code` on the item: `tax.code`, or the Product's when empty.
- `tax.taxable: false` writes Stripe's nontaxable code (`txcd_00000000`)
  instead.
- Tax location by facility isn't supported. Every line is taxed at the
  customer's address. See [Open Questions](#open-questions).

### Discounts

- `discountPercent` becomes a coupon on the item (`discounts[].coupon`).
- Coupons are standing, one per percentage, with fixed IDs
  (`datum-pct-10`), created on first use. They're never created per invoice.
- No negative lines: Stripe Tax won't calculate an invoice with them. A
  source that needs a credit uses a coupon or a credit note after
  finalization.

### Notes and metadata

- `note` goes in the item's metadata as `datum_note`. Metadata isn't printed
  on invoices, so customers never see it.
- `metadata` is copied onto the item, prefixed `datum_`, with
  `datum_line_key`, and `om_line_id` for OpenMeter's lines.
- Stripe allows 50 metadata keys of 500 characters each. Notes longer than
  that are refused at the source.

### Checking amounts

Before `Synced` is True, the controller compares each Stripe item's amount,
before discount, with the line's `amount`, and the invoice's subtotal with
the sum of the lines.

- A difference of a cent or more on any line sets `AmountsMatch=False` and
  leaves `Synced` False. openmeter-provider then doesn't tell OpenMeter the
  draft is synced, so nothing is issued.
- Quantity × unit price is the line's amount only when it divides exactly.
  The source must send lines that do, or a quantity of 1. Stripe's rounding
  is never allowed to change an amount.

## Previews

`purpose: Preview` previews without creating anything:

- It calls `invoices.create_preview` with the customer, `automatic_tax`
  and the lines as `invoice_items` (up to 250), written exactly as for a real
  invoice.
- It writes Stripe's lines, tax and totals into status, with
  `Previewed=True`.
- A customer without an address can be previewed with an address in the
  spec, passed as `customer_details`.
- The creator deletes it when done. A sweep deletes previews older than an
  hour.

`purpose: PreviewDraft` creates a real draft for staff to open in the Stripe
dashboard:

- It's written exactly as a real invoice, with `auto_advance: false`, and
  never finalized. CEL refuses `finalize` on it.
- Its metadata has `datum_purpose: PreviewDraft`, and its memo says it's a
  preview and not to send it.
- `status.dashboardUrl` links to it.
- It's deleted, with its Stripe draft, when its creator deletes it, or after
  a day by a sweep.
- openmeter-provider never projects drafts to `Invoice`s, and skips this
  metadata as well, so a preview draft can't reach the portals.

Previews have no PDF or hosted page in Stripe. The staff portal renders
`status` in a Datum invoice layout.

## Idempotency and Failure

- **Creating an invoice.** The idempotency key is the `StripeInvoice`'s UID
  plus `create`. `status.invoiceId` is written straight after. If the write
  is lost, the next pass lists the customer's draft invoices and adopts the
  one whose `datum_stripe_invoice` metadata names this resource.
- **Creating a line.** The key is the UID, the line key and the spec
  generation. Lines are matched to items by `datum_line_key`, so a retried
  pass updates rather than duplicates.
- **Finalizing** is safe to repeat: Stripe refuses to finalize twice, and the
  controller reads the state back.
- **Stripe errors.** Rate limits and 5xx requeue with backoff. A 4xx sets a
  condition with Stripe's message and stops until the spec changes.
- **stripe-provider down.** OpenMeter holds drafts until they're synced, so
  invoices are late, never lost or doubled.

## What Other Repos Do

### billing

- `ServicePricing` gains `tax.code`, `tax.taxable` and `tax.location`
  (`Customer` or `Facility`), part of the immutable spec. A customer pricing
  must match its list pricing's tax settings, as it must its SKU.
- A resource for charges staff add to an account: catalog pricing or custom
  description and amount, quantity, service period, tax settings, note and
  whether to invoice now. Its design belongs in billing's docs.
- The `InvoiceRequest` Preview gains Stripe's lines, tax and totals, and a
  link to a preview draft when one is asked for.

### openmeter-provider

- **Tax codes first.** Set each rate card's `taxConfig.stripe.code` from its
  pricing. That works with OpenMeter's Stripe app, before anything else
  here.
- **Custom Invoicing.** Install OpenMeter's Custom Invoicing app with the
  draft and issuing sync hooks on. Give each account moving over a new
  billing profile on it, switched at a billing boundary.
- **Lines.** For each OpenMeter draft waiting on sync, build a
  `StripeInvoice`:
  - each line's SKU, unit price, quantity and note from the pricing behind
    its rate card key, and the contract's quantity;
  - usage from OpenMeter's rated quantity and per-unit amount, one line per
    tier;
  - the period from OpenMeter's line;
  - discounts as `discountPercent`;
  - charges staff add, which reach OpenMeter as pending lines carrying our
    metadata.
- **Reporting back.** When `Synced` is True, call
  `/apps/custom-invoicing/{id}/draft/synchronized` with the Stripe invoice
  and item IDs. When OpenMeter is issuing, set `spec.finalize`. When
  `Finalized` is True, call `/issuing/synchronized`. Report payment status
  changes to `/payment/status`.
- **Previews.** The `InvoiceRequest` Preview builds the same lines from the
  Offer and creates a `StripeInvoice` with `purpose: Preview` or
  `PreviewDraft`.
- **Projection.** `Invoice` projection is unchanged. The Stripe invoice is
  OpenMeter's, because the draft sync reports its ID to OpenMeter and its
  metadata carries `om_invoice_id`.

### staff-portal

- Tax settings on catalog pricings and custom fees.
- The contract preview, from the `InvoiceRequest` Preview, with "Open as a
  Stripe draft".
- Adding charges to an account.

## Implementation Phases

Each phase ships on its own.

1. **Tax codes through OpenMeter.** billing: `ServicePricing` tax settings.
   openmeter-provider: rate card tax codes. Lines are taxed per product from
   here on.
2. **Products.** stripe-provider: `StripeProduct` and the product controller.
   Products exist in Stripe. Nothing on invoices changes.
3. **Previews.** stripe-provider: the preview controller. openmeter-provider
   and staff-portal: Stripe previews of contracts. This exercises line
   writing, tax and Products against real customers without billing anyone.
4. **Invoices on staging.** stripe-provider: the invoice controller and
   webhooks. openmeter-provider: Custom Invoicing and the line builder.
   Staging accounts move over, and each invoice is compared with what
   OpenMeter's Stripe app would have written.
5. **Charges staff add.** billing, openmeter-provider and staff-portal.
6. **Production, account by account**, at billing boundaries: internal
   accounts, then contracts, then everyone else.
7. **Retire SKUs in line names** in openmeter-provider.

## Observability

Metrics:

- `stripe_invoice_sync_seconds`: from `StripeInvoice` creation to `Synced`.
- `stripe_invoices{state,purpose}`.
- `stripe_invoice_amount_mismatches_total`.
- `stripe_api_errors_total{operation,code}`.
- `stripe_products{active}`, and `stripe_product_tax_code_conflicts`.

Alerts:

- a `StripeInvoice` with `purpose: Invoice` not `Synced` after an hour;
- any amount mismatch;
- a finalized invoice with no `Finalized` condition after an hour;
- preview drafts older than two days, meaning the sweep isn't running.

## Open Questions

- **Decimal quantities.** Prorated lines read "1 × amount" until we move to a
  Stripe API version with `quantity_decimal`. Is upgrading stripe-go worth it
  for this?
- **Tax by facility.** Stripe Tax taxes an invoice at the customer's
  location. A line taxed where its facility is may need its own invoice,
  with the facility as the shipping address. To confirm with Stripe.
- **Payment events in openmeter-provider.** It polls Stripe for invoice
  status today. It could read `StripeInvoice` status instead, which would
  remove its own Stripe calls for OpenMeter's invoices.
- **Where previews live.** A `StripeInvoice` per preview is simple but
  writes to etcd for every preview. If previews are frequent, an aggregated
  API or a direct call may suit better.

## References

- [Writing Our Own Stripe Invoices][enhancement] (enhancements)
- billing: `docs/enhancements/invoicing.md`, `docs/enhancements/contract-billing.md`
- openmeter-provider: `internal/controller/stripe_customer.go`,
  `internal/controller/stripe_invoice_sync.go`,
  `internal/controller/offer_mapping.go`
- [OpenMeter Custom Invoicing](https://openmeter.io/docs/integrations/external-invoicing/overview)
- Stripe: [invoice items](https://docs.stripe.com/api/invoiceitems/create),
  [create_preview](https://docs.stripe.com/api/invoices/create_preview),
  [tax codes](https://docs.stripe.com/tax/products-prices-tax-codes-tax-behavior),
  [Revenue Recognition](https://docs.stripe.com/revenue-recognition/methodology/subscriptions-and-invoicing),
  [idempotent requests](https://docs.stripe.com/api/idempotent_requests)

[enhancement]: https://github.com/datum-cloud/enhancements/blob/main/enhancements/platform/billing/stripe-invoicing/README.md
