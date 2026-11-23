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

Both are in `stripe.billing.miloapis.com/v1alpha1`, next to `StripeCustomer`.

### StripeProduct

One per SKU, in the namespace of its `ServicePricing`s (`milo-system`).
Named from the SKU, lower-cased, with a short hash of the exact SKU so case
and punctuation never collide (`sku-dc-cab-01-3f9a2c`). A list
`ServicePricing` without a SKU gets one of its own, named `pricing-<name>`,
which customer pricings based on it share.

```yaml
apiVersion: stripe.billing.miloapis.com/v1alpha1
kind: StripeProduct
metadata:
  name: sku-dc-cab-01-3f9a2c
  namespace: milo-system
spec:
  sku: DC-CAB-01              # exact SKU; or pricingRef for a pricing without one
status:
  productId: datum_3f9a...    # chosen by us, from the StripeProduct's name
  name: Structured cabling    # from the newest list pricing
  taxCode: txcd_20030000      # from the newest list pricing
  active: true                # false once no pricing stands for it
  pricings: [acme-cabling, cabling-v1, cabling-v2]
  conditions: [Ready, TaxCodeConflict]
```

The product controller creates these, and never deletes them: a Product
nothing sells any more is archived. Nobody else writes them. Other
controllers find a SKU's Product through `status.productId`.

The Product's ID is ours: `datum_` and a hash of the StripeProduct's
namespace and name. A create retried after a lost response finds the ID
taken and updates that Product instead of making another, so no search of
Stripe is needed.

### StripeInvoice

One per invoice to write, in the billing account's namespace, next to its
`StripeCustomer`. The creator owns it. For an OpenMeter invoice that's
openmeter-provider, and the name is derived from OpenMeter's invoice ID.

```yaml
apiVersion: stripe.billing.miloapis.com/v1alpha1
kind: StripeInvoice
metadata:
  name: om-01jd...            # from OpenMeter's invoice ID
  namespace: <account namespace>
spec:
  billingAccountRef: {name: acme}
  purpose: Invoice            # Invoice | Preview | PreviewDraft
  currency: USD
  source:                     # where the invoice came from: om_invoice_id metadata
    system: OpenMeter
    invoiceId: 01JD...
  lines:
    - key: 01JD...LINE        # stable; OpenMeter's line ID for its invoices
      description: Structured cabling
      servicePricingRef: {name: cabling-v2}  # its StripeProduct is the line's Product
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
      discount: "120.00"
      tax: "86.40"
  subtotal: "1200.00"
  discount: "120.00"
  tax: "86.40"
  total: "1166.40"
  hostedInvoiceUrl: ""        # set at finalization
  finalizedAt: null
  dueDate: null               # for an invoice that's sent
  paidAt: null
  amountPaid: "0.00"
  amountRemaining: "0.00"
  linesHash: 3f9a...          # the spec lines last written
  dashboardUrl: https://dashboard.stripe.com/invoices/in_...
  conditions: [Synced, Finalized, AmountsMatch, PaymentFailed]
  observedGeneration: 3
```

All three purposes are built. A preview's whole spec is immutable, so a new
preview is a new `StripeInvoice`. An `Invoice`'s lines can change until
`finalize` is set.

Rules, enforced by CEL:

- `purpose` and `billingAccountRef` are immutable.
- A line's period can't end before it starts, and a line that isn't taxable
  can't have a tax code.
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

Two reconcilers:

1. **ServicePricing → StripeProduct.** Each `ServicePricing` gets the
   `StripeProduct` that stands for it, created if it's missing.
2. **StripeProduct → Stripe.** For each `StripeProduct`, it reads the
   pricings it stands for and creates or updates the Stripe Product: name,
   `tax_code`, `active` and metadata (`datum_sku`, `datum_stripe_product`,
   `datum_managed_by`). Only fields that differ are sent. A
   `ServicePricing` event requeues its `StripeProduct`.

It sets `TaxCodeConflict` when list pricings with one SKU are taxed
differently. The newest one still decides: pricings are immutable, so a SKU
priced before tax codes existed would otherwise never get one. The
condition names the others, for finance to check.

The rules from the enhancement:

- One Product per SKU, not per price. Customer pricings (`basedOn`) never
  add a Product.
- The Product's name, description and tax code come from the newest list
  pricing with the SKU. A pricing without a description clears the
  Product's. A pricing's `notes` never reach Stripe.
- No Stripe Prices. Lines pass their unit price inline.
- A Product is archived (`active: false`) once no pricing uses its SKU, and
  reactivated if one does again. Stripe won't delete an invoiced Product.
  An archived Product keeps its last name, description and tax code.
- A pricing that isn't taxable makes its Product Stripe's nontaxable
  `txcd_00000000`. One with no tax leaves the Product without a code, so the
  account's default applies.

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
3. **Sync the lines.** Each invoice item carries its line's key
   (`datum_line_key`) and a hash of everything it's written from
   (`datum_line_hash`). An item whose line is gone, or whose hash no longer
   matches, is deleted; a line with no current item is created. Writing a
   changed line again, rather than updating its item, keeps one code path
   for every field. See [Writing Lines](#writing-lines). Every item is created
   with `invoice` set to this invoice. The draft's collection method follows
   the payment arrangement if it changed.
4. **Read back** amounts, discounts and tax per line, and totals, into
   status.
5. **Check amounts.** See [Checking amounts](#checking-amounts). `Synced`
   becomes True only when they match.
6. **Finalize** when `spec.finalize` is true. Steps 3 to 5 run once more
   first, even if the spec hasn't changed, so an item someone added to or
   removed from the draft in the Stripe dashboard is put right rather than
   billed; an item without a current `datum_line_key` is deleted. If
   `Synced` is True after that, finalize
   the invoice with `auto_advance: true`, then record `number`,
   `hostedInvoiceUrl` and `Finalized=True`. Stripe then charges the default
   payment method or sends the invoice, by its collection method.
7. **Read its state** from Stripe on every pass, and every six hours until
   it's paid or void: state, number, amounts, due and paid dates, and
   `PaymentFailed` while Stripe's attempts to charge it are failing. A draft
   someone deleted in Stripe becomes `state: Deleted` and isn't written
   again.

A `StripeInvoice` deleted before finalization deletes its Stripe draft
(finalizer). One deleted after finalization leaves the Stripe invoice alone:
finalized invoices are voided by staff, never deleted.

### Preview controller

Reconciles `StripeInvoice`s with `purpose: Preview` or `PreviewDraft`. See
[Previews](#previews).

### Webhooks

The webhook server today handles `setup_intent.*`, and `invoice.sent` for
the activity timeline: each time Stripe emails one of an account's
invoices, whoever created it, it records `InvoiceSent` on the account's
`BillingAccount` (see billing's
[Stripe Invoice Activity](https://github.com/milo-os/billing/blob/main/docs/enhancements/stripe-invoice-activity.md)).
That needs no `StripeInvoice`. For invoices it writes itself, it adds:

| Event | Effect |
|---|---|
| `invoice.finalized` | `state: Open`, number and URLs |
| `invoice.paid` | `state: Paid` |
| `invoice.payment_failed` | Condition `PaymentFailed`, with Stripe's reason |
| `invoice.voided` | `state: Void` |
| `invoice.marked_uncollectible` | `state: Uncollectible` |
| `invoice.deleted` | `state: Deleted` |

An event finds its `StripeInvoice` through `datum_stripe_invoice` (and
`datum_stripe_invoice_uid`) in the invoice's metadata, and stamps the
event's ID on its `stripe.billing.miloapis.com/refresh` annotation. The
controller then reads the invoice from Stripe: events say that something
changed, Stripe says what, so a late or repeated event can't set an old
state. `invoice.payment_succeeded`, `invoice.updated` and `invoice.sent` do
the same. Events for invoices without our metadata (staff-created ones) and
for preview drafts are ignored. The invoice controller also refreshes state
every six hours, so a missed event only delays a status. The webhook
endpoint has to be subscribed to these events in each Stripe account.

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

- `price_data.product`: the `StripeProduct` of the line's pricing.
- `price_data.unit_amount_decimal`: `unitAmount`, in cents.
- `price_data.tax_behavior`: `exclusive`, always.
- `quantity_decimal`: `quantity`, so a prorated line can read
  "0.43 × $1,000.00". This needs stripe-go v87 (API `2026-10-28.endive`),
  which stripe-provider is upgraded to first. The source still decides
  whether a prorated line has a decimal quantity or 1 × its amount, so that
  [amounts match](#checking-amounts).
- `description`: `description`, the price's display name. It's printed on
  the invoice, so it never carries the SKU.

**SKUs are internal.** They're for tracking and reporting, never printed on
an invoice. A line's SKU goes on its metadata (`datum_sku`, taken from its
pricing) and on its Product's metadata, where Stripe's reports and exports
can group by it. Stripe Products have no SKU field of their own, and invoices
don't print metadata.

### Service periods

- `period.start` and `period.end` from the line, on every line. Stripe's
  periods are inclusive.
- The source decides the period. stripe-provider only checks that the end
  isn't before the start.
- Revenue Recognition reads them, so a line without a period is refused
  rather than written without one.

### Tax

- The invoice has `automatic_tax.enabled`. Stripe Tax uses the customer's
  address, tax IDs and exemption from the `StripeCustomer`. Stripe won't
  finalize an invoice without the customer's location
  (`customer_tax_location_invalid`), so an account with no address of its
  own, as is usual for one paying by card, gives its customer the default
  card's billing address until it has one.
- A line taxed as its Product, nearly every line, is written with the
  Product and no `tax_code`. A line with a code of its own (`tax.code`) is
  written with that `tax_code` and no Product, which Stripe takes only
  without `price_data`; its Product is kept in `datum_product` metadata.
- `tax.taxable: false` writes Stripe's nontaxable code (`txcd_00000000`)
  instead.
- Tax location by facility isn't supported. Every line is taxed at the
  customer's address. Stripe's new tax locations (`tax_details.performance_location`
  on Products and lines) are for event tickets only, so they don't cover it.
  See [Open Questions](#open-questions).

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
- It writes Stripe's lines, discounts, tax and totals into status, with
  `Ready=True` (reason `Previewed`), and `AmountsMatch` saying whether each
  line came to its `amount`.
- It also writes the customer's `startingBalance`, which Stripe carries
  onto the invoice (positive for what they owe, such as an earlier invoice
  below Stripe's minimum charge, negative for credit), and the `amountDue`
  with it applied.
- It waits, with `Ready=False`, for the account's `StripeCustomer` and each
  line's `StripeProduct` to be ready.
- Not built yet: a customer without an address, previewed with one in the
  spec as `customer_details`. Today the account needs a Stripe customer.
- It deletes itself an hour after it's created (`status.expiresAt`), so the
  creator needn't.

`purpose: PreviewDraft` creates a real draft for staff to open in the Stripe
dashboard:

- It's written exactly as a real invoice, with `auto_advance: false`, and
  never finalized. CEL refuses `finalize` on it.
- Its metadata has `datum_purpose: PreviewDraft`, and its memo says it's a
  preview and not to send it.
- `status.dashboardUrl` links to it, under the dashboard's test pages for a
  test key.
- Its lines are created on the draft itself, so none is left pending on the
  customer for a real invoice to pick up. A draft made before a lost status
  write is found by its UID in metadata and finished, not made again.
- A finalizer deletes the Stripe draft with the `StripeInvoice`, which
  deletes itself a day after it's created. A draft someone finalized in
  Stripe can't be deleted: that's a `DraftNotDeleted` event, and the
  `StripeInvoice` still goes.
- openmeter-provider never projects drafts to `Invoice`s, and skips this
  metadata as well, so a preview draft can't reach the portals.

Previews have no PDF or hosted page in Stripe. The staff portal renders
`status` in a Datum invoice layout.

## Idempotency and Failure

- **Creating an invoice.** The idempotency key is the `StripeInvoice`'s UID
  plus `create`. `status.invoiceId` is written straight after. If the write
  is lost, the next pass lists the customer's draft invoices and adopts the
  one whose `datum_stripe_invoice` metadata names this resource.
- **Creating a line.** The key is the UID, the line key, the spec
  generation and the line's hash. Lines are matched to items by
  `datum_line_key`, so a retried pass updates rather than duplicates. The
  generation is there because Stripe replays a reused key for 24 hours with
  the item it first created, even if that item has since been deleted: a
  line removed and added back unchanged, which is a new generation, gets a
  new key and is written again. An item someone deletes in the Stripe
  dashboard is the one case a reused key can't fix: within the same
  generation and 24 hours, the line stays missing, `Synced` stays False on
  `AmountsDiffer`, and the invoice isn't finalized until the key expires or
  the spec changes.
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
  - each line's pricing, unit price, quantity and note from the pricing
    behind its rate card key, and the contract's quantity;
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

0. **Upgrade stripe-go to v87.** It pins Stripe API `2026-10-28.endive`,
   for decimal quantities, and moves the client to `stripe.Client`.
1. **Tax codes through OpenMeter.** billing: `ServicePricing` tax settings.
   openmeter-provider: rate card tax codes. Lines are taxed per product from
   here on.
2. **Products.** stripe-provider: `StripeProduct` and the product controller.
   Products exist in Stripe. Nothing on invoices changes.
3. **Previews.** stripe-provider: the preview controller. openmeter-provider
   and staff-portal: Stripe previews of contracts. This exercises line
   writing, tax and Products against real customers without billing anyone.
4. **Invoices on staging.** stripe-provider: the invoice controller and
   webhooks (built). openmeter-provider: Custom Invoicing and the line builder.
   Staging accounts move over, and each invoice is compared with what
   OpenMeter's Stripe app would have written.
5. **Charges staff add.** billing, openmeter-provider and staff-portal.
6. **Production, account by account**, at billing boundaries: internal
   accounts, then contracts, then everyone else.
7. ~~Retire SKUs in line names~~: done early. SKUs are internal, so
   openmeter-provider stopped putting them in line names before any
   release carried them.

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

- **Prorated lines.** Decimal quantities are available once stripe-go is
  on v87. Finance still chooses between "0.43 × $1,000.00" and
  "1 × $430.00" (an open question in the enhancement).
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
