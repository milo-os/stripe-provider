// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	stripego "github.com/stripe/stripe-go/v87"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// MetadataMiloInvoice is the metadata key naming the Milo Invoice a Stripe
// invoice is projected to. openmeter-provider writes it once it has
// projected the invoice, so the first send, which comes when the invoice is
// finalized, doesn't have it yet.
const MetadataMiloInvoice = "milo_invoice"

// invoicePayload is the subset of the Invoice JSON an invoice.sent event
// needs.
type invoicePayload struct {
	ID       string            `json:"id"`
	Number   string            `json:"number"`
	Customer json.RawMessage   `json:"customer"`
	Currency string            `json:"currency"`
	Total    int64             `json:"total"`
	DueDate  *int64            `json:"due_date"`
	Metadata map[string]string `json:"metadata"`
}

// customerID returns the invoice's customer id, which Stripe sends as a
// string unless the field was expanded into the Customer.
func (p *invoicePayload) customerID() string {
	var id string
	if err := json.Unmarshal(p.Customer, &id); err == nil {
		return id
	}
	var expanded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(p.Customer, &expanded); err == nil {
		return expanded.ID
	}
	return ""
}

func decodeInvoice(event *stripego.Event) (*invoicePayload, error) {
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return nil, errors.New("event has no data")
	}
	var inv invoicePayload
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		return nil, fmt.Errorf("decoding Invoice payload: %w", err)
	}
	if inv.ID == "" {
		return nil, errors.New("invoice payload missing id")
	}
	return &inv, nil
}

// handleInvoiceSent records InvoiceSent on the BillingAccount whose Stripe
// customer an invoice was emailed to, once per Stripe event. Each send,
// re-sends included, is its own Stripe event and so its own entry.
//
// The event goes on the account, not the Invoice: Stripe emails an invoice
// the moment it's finalized, before openmeter-provider has projected an
// Invoice for it, so the account is the only subject that always exists.
// The Invoice is named when the Stripe invoice's metadata already has it.
func (wh *Webhook) handleInvoiceSent(ctx context.Context, event *stripego.Event) error {
	inv, err := decodeInvoice(event)
	if err != nil {
		return err
	}
	if inv.Metadata[stripeinternal.MetadataPurpose] == string(stripev1alpha1.StripeInvoicePurposePreviewDraft) {
		// A preview draft is never meant to be sent; if someone sent one
		// anyway, it isn't a bill.
		return nil
	}
	account, err := wh.findBillingAccount(ctx, inv.customerID())
	if err != nil || account == nil {
		return err
	}
	return wh.createInvoiceSentEvent(ctx, event, inv, account)
}

// findBillingAccount returns the BillingAccount whose StripeCustomer is the
// Stripe customer customerID, or nil when it isn't one of ours.
func (wh *Webhook) findBillingAccount(ctx context.Context, customerID string) (*billingv1alpha1.BillingAccount, error) {
	if customerID == "" {
		return nil, nil
	}
	var customers stripev1alpha1.StripeCustomerList
	if err := wh.Client.List(ctx, &customers); err != nil {
		return nil, fmt.Errorf("listing StripeCustomers: %w", err)
	}
	for i := range customers.Items {
		sc := &customers.Items[i]
		if sc.Status.StripeCustomerID != customerID {
			continue
		}
		var account billingv1alpha1.BillingAccount
		key := types.NamespacedName{Namespace: sc.Namespace, Name: sc.Spec.BillingAccountRef.Name}
		if err := wh.Client.Get(ctx, key, &account); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("getting BillingAccount %s: %w", key, err)
		}
		return &account, nil
	}
	log.V(1).Info("ignoring invoice event for a customer without a StripeCustomer", "customer", customerID)
	return nil, nil
}

// createInvoiceSentEvent creates the InvoiceSent Event itself rather than
// through the EventRecorder, for two reasons the recorder can't serve:
//
//   - Its name is derived from the Stripe event id, so a redelivery (after a
//     restart, or to another replica, neither of which the in-memory dedupe
//     sees) gets AlreadyExists and records nothing.
//   - Its timestamps are when Stripe sent the email, not when we got the
//     event.
//
// Otherwise it's what the recorder would write.
func (wh *Webhook) createInvoiceSentEvent(
	ctx context.Context,
	event *stripego.Event,
	inv *invoicePayload,
	account *billingv1alpha1.BillingAccount,
) error {
	annotations := map[string]string{}
	maps.Copy(annotations, billingv1alpha1.OrganizationScopeAnnotations(account.Namespace))
	maps.Copy(annotations, invoiceSentEventDetail(inv))

	at := metav1.NewTime(time.Unix(event.Created, 0))
	if event.Created == 0 {
		at = metav1.Now()
	}
	host, _ := os.Hostname()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:        invoiceSentEventName(account.Name, event.ID),
			Namespace:   account.Namespace,
			Annotations: annotations,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion:      billingv1alpha1.GroupVersion.String(),
			Kind:            "BillingAccount",
			Namespace:       account.Namespace,
			Name:            account.Name,
			UID:             account.UID,
			ResourceVersion: account.ResourceVersion,
		},
		Reason:              billingv1alpha1.EventReasonInvoiceSent,
		Message:             "Invoice emailed to the customer.",
		Type:                corev1.EventTypeNormal,
		Source:              corev1.EventSource{Component: eventRecorderName, Host: host},
		FirstTimestamp:      at,
		LastTimestamp:       at,
		Count:               1,
		ReportingController: eventRecorderName,
		ReportingInstance:   host,
	}
	if err := wh.Client.Create(ctx, ev); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.V(1).Info("invoice send already recorded", "event", event.ID)
			return nil
		}
		return fmt.Errorf("creating InvoiceSent event for BillingAccount %s/%s: %w", account.Namespace, account.Name, err)
	}
	return nil
}

// invoiceSentEventName is the Event's name: the account's, then a hash of
// the Stripe event id (which isn't a valid name itself), within the 253
// characters a name may have.
func invoiceSentEventName(account, stripeEventID string) string {
	sum := sha256.Sum256([]byte(stripeEventID))
	suffix := "." + hex.EncodeToString(sum[:])[:16]
	if max := 253 - len(suffix); len(account) > max {
		account = account[:max]
	}
	return account + suffix
}

// invoiceSentEventDetail describes the sent invoice: its number, total and
// due date, and its Milo Invoice when it has been projected. The customer's
// email address never goes on the Event.
func invoiceSentEventDetail(inv *invoicePayload) map[string]string {
	detail := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			detail[k] = v
		}
	}
	set(billingv1alpha1.EventAnnotationInvoiceNumber, inv.Number)
	set(billingv1alpha1.EventAnnotationInvoice, inv.Metadata[MetadataMiloInvoice])
	if inv.Currency != "" {
		set(billingv1alpha1.EventAnnotationCurrency, strings.ToUpper(inv.Currency))
		set(billingv1alpha1.EventAnnotationAmount, formatMinorUnits(inv.Total, inv.Currency))
	}
	if inv.DueDate != nil && *inv.DueDate > 0 {
		set(billingv1alpha1.EventAnnotationDueDate, billingv1alpha1.EventTime(time.Unix(*inv.DueDate, 0)))
	}
	return detail
}

// zeroDecimalCurrencies are the currencies Stripe counts in whole units.
// https://docs.stripe.com/currencies#zero-decimal
var zeroDecimalCurrencies = map[string]bool{
	"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true,
	"krw": true, "mga": true, "pyg": true, "rwf": true, "ugx": true, "vnd": true,
	"vuv": true, "xaf": true, "xof": true, "xpf": true,
}

// formatMinorUnits turns a Stripe amount in the currency's minor unit into
// the decimal string Invoice status uses ("482.19", or "1000" for JPY).
func formatMinorUnits(amount int64, currency string) string {
	if zeroDecimalCurrencies[strings.ToLower(currency)] {
		return decimal.NewFromInt(amount).String()
	}
	return decimal.New(amount, -2).StringFixed(2)
}
