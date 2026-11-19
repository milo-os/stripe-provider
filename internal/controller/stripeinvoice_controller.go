// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

const (
	// previewLifetime is how long a Preview StripeInvoice is kept: long
	// enough to read, short enough that previews don't pile up.
	previewLifetime = time.Hour
	// previewDraftLifetime is how long a PreviewDraft, and its Stripe
	// draft, are kept for staff to open.
	previewDraftLifetime = 24 * time.Hour
	// stripeInvoiceWaitRequeue is how soon a StripeInvoice waiting for its
	// customer or a Product is tried again.
	stripeInvoiceWaitRequeue = 30 * time.Second

	// StripeInvoiceDraftFinalizer deletes a PreviewDraft's Stripe draft
	// before the StripeInvoice goes.
	StripeInvoiceDraftFinalizer = "stripe.billing.miloapis.com/delete-draft"

	// previewDraftMemo is a preview draft's memo, shown to anyone who
	// opens it in the dashboard.
	previewDraftMemo = "Preview only, from Datum's staff portal. Don't send or finalize it: it's deleted automatically."
)

// stripeInvoicer is the narrow surface of the Stripe SDK wrapper the
// StripeInvoice reconciler needs, so tests can stand in for Stripe.
type stripeInvoicer interface {
	PreviewInvoice(ctx context.Context, d stripeinternal.InvoiceDraft) (*stripeinternal.InvoiceResult, error)
	EnsureDraftInvoice(ctx context.Context, existingID string, d stripeinternal.InvoiceDraft) (*stripeinternal.InvoiceResult, error)
	DeleteDraftInvoice(ctx context.Context, id string) error
	DashboardInvoiceURL(id string) string
}

// StripeInvoiceReconciler has Stripe work out StripeInvoices: a preview,
// with nothing created, or a draft for staff to open, deleted with the
// StripeInvoice. Each StripeInvoice deletes itself once it expires.
type StripeInvoiceReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder

	// ProviderConfigName names the StripeProviderConfig for SDK
	// credentials, as for the other reconcilers.
	ProviderConfigName string

	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// stripeClientFactory lets tests stand in for Stripe. Production
	// leaves it nil.
	stripeClientFactory func() stripeInvoicer
}

// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeinvoices,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeinvoices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeinvoices/finalizers,verbs=update
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripecustomers,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproducts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=servicepricings,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *StripeInvoiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var inv stripev1alpha1.StripeInvoice
	if err := r.Client.Get(ctx, req.NamespacedName, &inv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !inv.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &inv)
	}

	expiresAt := inv.CreationTimestamp.Add(lifetime(inv.Spec.Purpose))
	if !r.now().Before(expiresAt) {
		log.FromContext(ctx).V(1).Info("StripeInvoice expired; deleting it", "stripeInvoice", inv.Name)
		return ctrl.Result{}, client.IgnoreNotFound(r.Client.Delete(ctx, &inv))
	}

	if inv.Spec.Purpose == stripev1alpha1.StripeInvoicePurposePreviewDraft &&
		controllerutil.AddFinalizer(&inv, StripeInvoiceDraftFinalizer) {
		if err := r.Client.Update(ctx, &inv); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	untilExpiry := ctrl.Result{RequeueAfter: expiresAt.Sub(r.now())}
	if inv.Status.ObservedGeneration == inv.Generation &&
		apimeta.IsStatusConditionTrue(inv.Status.Conditions, stripev1alpha1.StripeInvoiceConditionReady) {
		return untilExpiry, nil
	}

	draft, waiting, err := r.invoiceDraft(ctx, &inv)
	if err != nil {
		return ctrl.Result{}, err
	}
	if waiting != nil {
		return ctrl.Result{RequeueAfter: stripeInvoiceWaitRequeue}, r.setStatus(ctx, &inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
			r.setReady(s, &inv, metav1.ConditionFalse, waiting.reason, waiting.message)
		})
	}

	stripe, err := r.buildStripeClient(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	var result *stripeinternal.InvoiceResult
	reason := "Previewed"
	if inv.Spec.Purpose == stripev1alpha1.StripeInvoicePurposePreviewDraft {
		reason = "Drafted"
		result, err = stripe.EnsureDraftInvoice(ctx, inv.Status.InvoiceID, *draft)
	} else {
		result, err = stripe.PreviewInvoice(ctx, *draft)
	}
	if err != nil {
		statusErr := r.setStatus(ctx, &inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
			if result != nil && result.ID != "" && inv.Spec.Purpose == stripev1alpha1.StripeInvoicePurposePreviewDraft {
				// Recorded so the finalizer deletes a half-made draft.
				s.InvoiceID = result.ID
			}
			r.setReady(s, &inv, metav1.ConditionFalse, "StripeError", err.Error())
		})
		return ctrl.Result{}, errors.Join(err, statusErr)
	}

	return untilExpiry, r.setStatus(ctx, &inv, func(s *stripev1alpha1.StripeInvoiceStatus) {
		if inv.Spec.Purpose == stripev1alpha1.StripeInvoicePurposePreviewDraft {
			s.InvoiceID = result.ID
			s.DashboardURL = stripe.DashboardInvoiceURL(result.ID)
		}
		s.Subtotal = majorUnits(result.Subtotal)
		s.Discount = majorUnits(result.Discount)
		s.Tax = majorUnits(result.Tax)
		s.Total = majorUnits(result.Total)
		s.Lines = nil
		for _, l := range result.Lines {
			s.Lines = append(s.Lines, stripev1alpha1.StripeInvoiceLineStatus{
				Key: l.Key, Amount: majorUnits(l.Amount), Discount: majorUnits(l.Discount), Tax: majorUnits(l.Tax),
			})
		}
		expires := metav1.NewTime(expiresAt)
		s.ExpiresAt = &expires
		r.setAmountsMatch(s, &inv, result)
		r.setReady(s, &inv, metav1.ConditionTrue, reason, "Stripe worked out the invoice")
	})
}

// waitReason is why a StripeInvoice can't be worked out yet.
type waitReason struct {
	reason, message string
}

// invoiceDraft builds the Stripe invoice from the spec: the account's
// customer, and each line's Product from its pricing's StripeProduct.
func (r *StripeInvoiceReconciler) invoiceDraft(ctx context.Context, inv *stripev1alpha1.StripeInvoice) (*stripeinternal.InvoiceDraft, *waitReason, error) {
	var customer stripev1alpha1.StripeCustomer
	key := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.BillingAccountRef.Name}
	if err := r.Client.Get(ctx, key, &customer); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &waitReason{"WaitingForCustomer", fmt.Sprintf("BillingAccount %s has no Stripe customer yet", key.Name)}, nil
		}
		return nil, nil, fmt.Errorf("getting StripeCustomer: %w", err)
	}
	if customer.Status.StripeCustomerID == "" {
		return nil, &waitReason{"WaitingForCustomer", fmt.Sprintf("BillingAccount %s's Stripe customer isn't ready", key.Name)}, nil
	}

	currency := inv.Spec.Currency
	if currency == "" {
		currency = "USD"
	}
	draft := &stripeinternal.InvoiceDraft{
		CustomerID:     customer.Status.StripeCustomerID,
		Currency:       currency,
		Description:    previewDraftMemo,
		IdempotencyKey: "stripeinvoice-" + string(inv.UID),
		Metadata: map[string]string{
			stripeinternal.MetadataStripeInvoice:    inv.Namespace + "/" + inv.Name,
			stripeinternal.MetadataStripeInvoiceUID: string(inv.UID),
			stripeinternal.MetadataPurpose:          string(inv.Spec.Purpose),
		},
	}
	for _, l := range inv.Spec.Lines {
		productID, sku, waiting, err := r.lineProduct(ctx, l.ServicePricingRef)
		if err != nil || waiting != nil {
			return nil, waiting, err
		}
		metadata := l.Metadata
		if sku != "" {
			// The SKU is internal, for tracking and reporting: it goes on the
			// line's metadata, never its description.
			metadata = map[string]string{"sku": sku}
			for k, v := range l.Metadata {
				metadata[k] = v
			}
		}
		line := stripeinternal.InvoiceLine{
			Key:         l.Key,
			Description: l.Description,
			ProductID:   productID,
			Quantity:    decimal.RequireFromString(l.Quantity),
			UnitAmount:  decimal.RequireFromString(l.UnitAmount),
			Start:       l.Period.Start.Time,
			End:         l.Period.End.Time,
			Note:        l.Note,
			Metadata:    metadata,
		}
		if t := l.Tax; t != nil {
			switch {
			case t.Taxable != nil && !*t.Taxable:
				line.TaxCode = stripeinternal.NontaxableTaxCode
			default:
				line.TaxCode = t.Code
			}
		}
		if l.DiscountPercent != "" {
			line.DiscountPercent = decimal.RequireFromString(l.DiscountPercent)
		}
		draft.Lines = append(draft.Lines, line)
	}
	return draft, nil, nil
}

// lineProduct is the Stripe Product of a pricing, its StripeProduct's, and
// the pricing's SKU.
func (r *StripeInvoiceReconciler) lineProduct(ctx context.Context, ref stripev1alpha1.ServicePricingRef) (string, string, *waitReason, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = billingv1alpha1.DefaultServicePricingNamespace
	}
	var sp billingv1alpha1.ServicePricing
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &sp); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", &waitReason{"WaitingForProduct", fmt.Sprintf("ServicePricing %s/%s doesn't exist", ns, ref.Name)}, nil
		}
		return "", "", nil, fmt.Errorf("getting ServicePricing %s/%s: %w", ns, ref.Name, err)
	}
	var product stripev1alpha1.StripeProduct
	name := stripeProductName(&sp)
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &product); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", &waitReason{"WaitingForProduct", fmt.Sprintf("ServicePricing %s has no StripeProduct yet", ref.Name)}, nil
		}
		return "", "", nil, fmt.Errorf("getting StripeProduct %s: %w", name, err)
	}
	if product.Status.ProductID == "" ||
		!apimeta.IsStatusConditionTrue(product.Status.Conditions, stripev1alpha1.StripeProductConditionReady) {
		return "", "", &waitReason{"WaitingForProduct", fmt.Sprintf("StripeProduct %s isn't ready", name)}, nil
	}
	return product.Status.ProductID, sp.Spec.SKU, nil, nil
}

// finalize deletes a PreviewDraft's Stripe draft, then lets the
// StripeInvoice go. A draft someone finalized can't be deleted: that's
// reported, and the StripeInvoice still goes.
func (r *StripeInvoiceReconciler) finalize(ctx context.Context, inv *stripev1alpha1.StripeInvoice) error {
	if !controllerutil.ContainsFinalizer(inv, StripeInvoiceDraftFinalizer) {
		return nil
	}
	if id := inv.Status.InvoiceID; id != "" {
		stripe, err := r.buildStripeClient(ctx)
		if err != nil {
			return err
		}
		if err := stripe.DeleteDraftInvoice(ctx, id); err != nil {
			if !errors.Is(err, stripeinternal.ErrInvoiceNotDraft) {
				return err
			}
			r.event(inv, "Warning", "DraftNotDeleted",
				fmt.Sprintf("Preview draft %s was finalized in Stripe, so it can't be deleted; void it there", id))
		}
	}
	controllerutil.RemoveFinalizer(inv, StripeInvoiceDraftFinalizer)
	return client.IgnoreNotFound(r.Client.Update(ctx, inv))
}

func (r *StripeInvoiceReconciler) setStatus(ctx context.Context, inv *stripev1alpha1.StripeInvoice, mutate func(*stripev1alpha1.StripeInvoiceStatus)) error {
	mutate(&inv.Status)
	inv.Status.ObservedGeneration = inv.Generation
	if err := r.Client.Status().Update(ctx, inv); err != nil {
		return fmt.Errorf("updating StripeInvoice status: %w", err)
	}
	return nil
}

func (r *StripeInvoiceReconciler) setReady(s *stripev1alpha1.StripeInvoiceStatus, inv *stripev1alpha1.StripeInvoice, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type: stripev1alpha1.StripeInvoiceConditionReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: inv.Generation,
	})
}

// setAmountsMatch checks Stripe's amount for each line, before discounts,
// against the amount the line should come to.
func (r *StripeInvoiceReconciler) setAmountsMatch(s *stripev1alpha1.StripeInvoiceStatus, inv *stripev1alpha1.StripeInvoice, result *stripeinternal.InvoiceResult) {
	got := map[string]int64{}
	for _, l := range result.Lines {
		got[l.Key] = l.Amount
	}
	cond := metav1.Condition{
		Type: stripev1alpha1.StripeInvoiceConditionAmountsMatch, Status: metav1.ConditionTrue,
		Reason: "Match", Message: "Every line comes to its amount", ObservedGeneration: inv.Generation,
	}
	for _, l := range inv.Spec.Lines {
		want := decimal.RequireFromString(l.Amount).Shift(2).Round(0).IntPart()
		if amount, ok := got[l.Key]; !ok || amount != want {
			cond.Status = metav1.ConditionFalse
			cond.Reason = "Differ"
			cond.Message = fmt.Sprintf("Line %s comes to %s in Stripe, not %s", l.Key, majorUnits(amount), l.Amount)
			break
		}
	}
	apimeta.SetStatusCondition(&s.Conditions, cond)
}

func (r *StripeInvoiceReconciler) buildStripeClient(ctx context.Context) (stripeInvoicer, error) {
	if r.stripeClientFactory != nil {
		return r.stripeClientFactory(), nil
	}
	cfg, err := stripeinternal.ResolveConfig(ctx, r.Client, r.ProviderConfigName)
	if err != nil {
		return nil, fmt.Errorf("resolving StripeProviderConfig: %w", err)
	}
	return stripeinternal.NewClient(cfg), nil
}

func (r *StripeInvoiceReconciler) event(obj *stripev1alpha1.StripeInvoice, kind, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, kind, reason, message)
	}
}

func (r *StripeInvoiceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager wires the reconciler.
func (r *StripeInvoiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("stripe-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("stripeinvoice").
		For(&stripev1alpha1.StripeInvoice{}).
		Complete(r)
}

func lifetime(p stripev1alpha1.StripeInvoicePurpose) time.Duration {
	if p == stripev1alpha1.StripeInvoicePurposePreviewDraft {
		return previewDraftLifetime
	}
	return previewLifetime
}

// majorUnits renders an amount in cents as a decimal string, e.g. "12.50".
func majorUnits(cents int64) string {
	return decimal.New(cents, -2).StringFixed(2)
}
