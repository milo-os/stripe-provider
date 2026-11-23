// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

const (
	// invoiceRefreshInterval is how often an invoice that's still open is
	// read from Stripe again, so a missed event only delays its state.
	invoiceRefreshInterval = 6 * time.Hour

	// StripeInvoiceRefreshAnnotation is touched by the webhook on each of an
	// invoice's Stripe events, to have it read again at once.
	StripeInvoiceRefreshAnnotation = "stripe.billing.miloapis.com/refresh"
)

// reconcileInvoice writes a real invoice: its Stripe draft is kept in step
// with the spec's lines until spec.finalize, when it's finalized, and then
// its state is read from Stripe until it's paid, voided or uncollectible.
// See docs/architecture/invoicing.md#invoice-controller.
func (r *StripeInvoiceReconciler) reconcileInvoice(ctx context.Context, inv *stripev1alpha1.StripeInvoice) (ctrl.Result, error) {
	if controllerutil.AddFinalizer(inv, StripeInvoiceDraftFinalizer) {
		if err := r.Client.Update(ctx, inv); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}
	stripe, err := r.buildStripeClient(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !invoiceFinalized(inv) && inv.Status.State != stripev1alpha1.StripeInvoiceStateDeleted {
		// The draft is written again just before it's finalized, even if
		// nothing changed here, so a line someone added or removed in the
		// Stripe dashboard is put right rather than billed.
		if needsSync(inv) || inv.Spec.Finalize {
			result, waiting, err := r.syncInvoice(ctx, inv, stripe)
			if waiting != nil {
				return ctrl.Result{RequeueAfter: stripeInvoiceWaitRequeue}, r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
					setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionFalse, waiting.reason, waiting.message)
				})
			}
			if err != nil {
				return ctrl.Result{}, err
			}
			if result == nil {
				// The draft is gone from Stripe; status says so.
				return ctrl.Result{}, nil
			}
		}
		if inv.Spec.Finalize && apimeta.IsStatusConditionTrue(inv.Status.Conditions, stripev1alpha1.StripeInvoiceConditionSynced) {
			result, err := stripe.FinalizeInvoice(ctx, inv.Status.InvoiceID, "stripeinvoice-"+string(inv.UID))
			if err != nil {
				return ctrl.Result{}, errors.Join(err, r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
					setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionFinalized, metav1.ConditionFalse, "StripeError", stripeinternal.ErrorMessage(err))
				}))
			}
			if err := r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) { applyInvoiceState(s, inv, result) }); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: invoiceRefreshInterval}, nil
		}
	}

	return r.refreshInvoice(ctx, inv, stripe)
}

// syncInvoice writes the draft and its lines, and reads back Stripe's
// amounts. It returns no result when the draft is gone from Stripe, having
// recorded that.
func (r *StripeInvoiceReconciler) syncInvoice(ctx context.Context, inv *stripev1alpha1.StripeInvoice, stripe stripeInvoicer) (*stripeinternal.InvoiceResult, *waitReason, error) {
	draft, waiting, err := r.invoiceDraft(ctx, inv)
	if err != nil || waiting != nil {
		return nil, waiting, err
	}
	result, err := stripe.EnsureDraftInvoice(ctx, inv.Status.InvoiceID, *draft)
	if errors.Is(err, stripeinternal.ErrInvoiceMissing) {
		return nil, nil, r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
			s.State = stripev1alpha1.StripeInvoiceStateDeleted
			setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionFalse, "Deleted",
				fmt.Sprintf("Stripe draft %s was deleted in Stripe", inv.Status.InvoiceID))
		})
	}
	if err != nil {
		return nil, nil, errors.Join(err, r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
			if result != nil && result.ID != "" {
				// Recorded so the next attempt, and the finalizer, find it.
				s.InvoiceID = result.ID
			}
			setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionFalse, "StripeError", stripeinternal.ErrorMessage(err))
		}))
	}
	return result, nil, r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
		s.InvoiceID = result.ID
		s.DashboardURL = stripe.DashboardInvoiceURL(result.ID)
		applyAmounts(s, result)
		applyInvoiceState(s, inv, result)
		if result.Status != "draft" {
			// Finalized in Stripe by someone else: its lines are what they are.
			setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionFalse, "Finalized",
				"The invoice was finalized in Stripe before its lines were last written")
			return
		}
		s.LinesHash = linesHash(inv.Spec.Lines)
		r.setAmountsMatch(s, inv, result)
		if apimeta.IsStatusConditionTrue(s.Conditions, stripev1alpha1.StripeInvoiceConditionAmountsMatch) {
			setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionTrue, "Synced",
				"The Stripe draft holds the invoice's lines")
		} else {
			msg := apimeta.FindStatusCondition(s.Conditions, stripev1alpha1.StripeInvoiceConditionAmountsMatch).Message
			setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionFalse, "AmountsDiffer", msg)
		}
	})
}

// refreshInvoice reads the invoice's state from Stripe, so a change made
// there (finalized, paid, voided, or a draft deleted) reaches status, and
// keeps reading it every few hours until it's settled.
func (r *StripeInvoiceReconciler) refreshInvoice(ctx context.Context, inv *stripev1alpha1.StripeInvoice, stripe stripeInvoicer) (ctrl.Result, error) {
	if inv.Status.State == stripev1alpha1.StripeInvoiceStateDeleted || inv.Status.InvoiceID == "" {
		return ctrl.Result{}, nil
	}
	result, err := stripe.RetrieveInvoice(ctx, inv.Status.InvoiceID)
	if errors.Is(err, stripeinternal.ErrInvoiceMissing) && !invoiceFinalized(inv) {
		return ctrl.Result{}, r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
			s.State = stripev1alpha1.StripeInvoiceStateDeleted
			setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionFalse, "Deleted",
				fmt.Sprintf("Stripe draft %s was deleted in Stripe", inv.Status.InvoiceID))
		})
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	err = r.setStatus(ctx, inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
		applyAmounts(s, result)
		applyInvoiceState(s, inv, result)
		if invoiceFinalized(&stripev1alpha1.StripeInvoice{Status: *s}) && s.LinesHash != "" && s.LinesHash != linesHash(inv.Spec.Lines) {
			setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionSynced, metav1.ConditionFalse, "Finalized",
				"The lines changed after the invoice was finalized; Stripe keeps the ones it was finalized with")
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	switch inv.Status.State {
	case stripev1alpha1.StripeInvoiceStatePaid, stripev1alpha1.StripeInvoiceStateVoid:
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: invoiceRefreshInterval}, nil
}

// needsSync reports whether the draft has to be written: it never has been,
// its lines changed, or the last attempt didn't finish. Asking for it to be
// finalized isn't a change to write.
func needsSync(inv *stripev1alpha1.StripeInvoice) bool {
	return inv.Status.InvoiceID == "" ||
		inv.Status.LinesHash != linesHash(inv.Spec.Lines) ||
		!apimeta.IsStatusConditionTrue(inv.Status.Conditions, stripev1alpha1.StripeInvoiceConditionSynced)
}

func invoiceFinalized(inv *stripev1alpha1.StripeInvoice) bool {
	switch inv.Status.State {
	case "", stripev1alpha1.StripeInvoiceStateDraft, stripev1alpha1.StripeInvoiceStateDeleted:
		return false
	}
	return true
}

// setCollection has the invoice charged or sent by the account's payment
// arrangement: sent, due its net days later, on payment terms; otherwise
// charged to the default payment method.
func (r *StripeInvoiceReconciler) setCollection(ctx context.Context, inv *stripev1alpha1.StripeInvoice, draft *stripeinternal.InvoiceDraft) error {
	var account billingv1alpha1.BillingAccount
	key := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.BillingAccountRef.Name}
	if err := r.Client.Get(ctx, key, &account); err != nil {
		if apierrors.IsNotFound(err) {
			draft.CollectionMethod = stripeinternal.CollectionChargeAutomatically
			return nil
		}
		return fmt.Errorf("getting BillingAccount %s: %w", key, err)
	}
	draft.CollectionMethod = stripeinternal.CollectionChargeAutomatically
	if hasInvoiceArrangement(&account) {
		draft.CollectionMethod = stripeinternal.CollectionSendInvoice
		draft.DaysUntilDue = int64(billingv1alpha1.StandardPaymentTermsNetDays)
		if terms := account.Status.PaymentArrangement.Invoice; terms != nil && terms.NetDays > 0 {
			draft.DaysUntilDue = int64(terms.NetDays)
		}
	}
	return nil
}

// applyAmounts copies Stripe's amounts into status.
func applyAmounts(s *stripev1alpha1.StripeInvoiceStatus, result *stripeinternal.InvoiceResult) {
	s.Subtotal = majorUnits(result.Subtotal)
	s.Discount = majorUnits(result.Discount)
	s.Tax = majorUnits(result.Tax)
	s.Total = majorUnits(result.Total)
	s.StartingBalance = majorUnits(result.StartingBalance)
	s.AmountDue = majorUnits(result.AmountDue)
	s.AmountPaid = majorUnits(result.AmountPaid)
	s.AmountRemaining = majorUnits(result.AmountRemaining)
	s.Lines = nil
	for _, l := range result.Lines {
		s.Lines = append(s.Lines, stripev1alpha1.StripeInvoiceLineStatus{
			Key: l.Key, InvoiceItemID: l.InvoiceItemID,
			Amount: majorUnits(l.Amount), Discount: majorUnits(l.Discount), Tax: majorUnits(l.Tax),
		})
	}
}

// applyInvoiceState copies where the invoice stands in Stripe into status.
func applyInvoiceState(s *stripev1alpha1.StripeInvoiceStatus, inv *stripev1alpha1.StripeInvoice, result *stripeinternal.InvoiceResult) {
	s.State = invoiceState(result.Status)
	s.Number = result.Number
	s.HostedInvoiceURL = result.HostedInvoiceURL
	s.FinalizedAt = metaTime(result.FinalizedAt)
	s.PaidAt = metaTime(result.PaidAt)
	s.DueDate = metaTime(result.DueDate)
	s.AmountPaid = majorUnits(result.AmountPaid)
	s.AmountRemaining = majorUnits(result.AmountRemaining)
	if invoiceFinalized(&stripev1alpha1.StripeInvoice{Status: *s}) {
		setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionFinalized, metav1.ConditionTrue, "Finalized",
			fmt.Sprintf("Stripe finalized invoice %s", result.Number))
	}
	switch {
	case s.State == stripev1alpha1.StripeInvoiceStateOpen && result.AttemptCount > 0:
		msg := fmt.Sprintf("Stripe couldn't charge the invoice after %d attempt(s)", result.AttemptCount)
		if !result.NextPaymentAttempt.IsZero() {
			msg += "; it tries again at " + result.NextPaymentAttempt.Format(time.RFC3339)
		}
		setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionPaymentFailed, metav1.ConditionTrue, "PaymentFailed", msg)
	case apimeta.FindStatusCondition(s.Conditions, stripev1alpha1.StripeInvoiceConditionPaymentFailed) != nil:
		setCondition(s, inv, stripev1alpha1.StripeInvoiceConditionPaymentFailed, metav1.ConditionFalse, "NoFailure",
			"No payment attempt is failing")
	}
}

func invoiceState(stripeStatus string) stripev1alpha1.StripeInvoiceState {
	switch stripeStatus {
	case "open":
		return stripev1alpha1.StripeInvoiceStateOpen
	case "paid":
		return stripev1alpha1.StripeInvoiceStatePaid
	case "void":
		return stripev1alpha1.StripeInvoiceStateVoid
	case "uncollectible":
		return stripev1alpha1.StripeInvoiceStateUncollectible
	}
	return stripev1alpha1.StripeInvoiceStateDraft
}

func setCondition(s *stripev1alpha1.StripeInvoiceStatus, inv *stripev1alpha1.StripeInvoice, kind string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: inv.Generation,
	})
}

func metaTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}

// linesHash is a hash of the spec lines, to see when they change.
func linesHash(lines []stripev1alpha1.StripeInvoiceLine) string {
	b, _ := json.Marshal(lines)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
