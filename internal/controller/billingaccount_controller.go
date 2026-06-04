// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// BillingAccountReconciler keeps the Stripe Customer record in sync with
// the owning BillingAccount's spec. The StripePaymentMethod reconciler
// stamps Customer details on initial creation, but never re-runs against
// the same Customer when the BillingAccount changes later — so edits to
// `spec.contactInfo` (tax IDs, address, business name, invoice
// recipients) never propagate to Stripe.
//
// This reconciler watches BillingAccount, finds the Stripe Customer ID
// previously associated with the account (via the same dedup lookup the
// StripePaymentMethod reconciler uses), and re-runs EnsureCustomer with
// the current spec to push the latest values up.
//
// When the BillingAccount has no Stripe Customer yet (i.e. no
// StripePaymentMethod has reached AwaitingConfirmation) the reconciler
// is a no-op: there is nothing to update yet, and the
// StripePaymentMethod reconciler will pick up the freshest details from
// the BillingAccount the next time a PaymentMethod is created.
type BillingAccountReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme

	// ProviderConfigName names the StripeProviderConfig the reconciler
	// uses for SDK credentials. Set from the operator flag, same value
	// the StripePaymentMethodReconciler uses.
	ProviderConfigName string

	// stripeClientFactory builds the Stripe SDK wrapper from a resolved
	// config. Indirection exists so unit tests can substitute a stub
	// that records EnsureCustomer calls without hitting the real
	// Stripe API. Production wiring (cmd/stripe-provider) leaves this
	// nil and the reconciler builds the real client.
	stripeClientFactory func(cfg *stripeinternal.ResolvedConfig) stripeCustomerEnsurer
}

// stripeCustomerEnsurer is the narrow surface of the Stripe SDK wrapper
// this reconciler needs. Defined here (rather than as a method on
// stripeinternal.Client) so tests can swap it out without exporting
// extra surface from the stripe package.
type stripeCustomerEnsurer interface {
	EnsureCustomer(ctx context.Context, existingID, billingAccountName string, details stripeinternal.CustomerDetails) (string, error)
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *BillingAccountReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ba billingv1alpha1.BillingAccount
	if err := r.Client.Get(ctx, req.NamespacedName, &ba); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting BillingAccount: %w", err)
	}

	if !ba.DeletionTimestamp.IsZero() {
		// Deletion of the Stripe Customer is the StripePaymentMethod
		// reconciler's responsibility (it owns the cleanup finalizer).
		// Nothing to do here.
		return ctrl.Result{}, nil
	}

	// Locate the Stripe Customer ID previously stamped onto a sibling
	// StripePaymentMethod for this BillingAccount. If none exists the
	// account has no Stripe-side presence yet; bail until a
	// PaymentMethod is created.
	customerID, err := findExistingCustomerID(ctx, r.Client, ba.Namespace, ba.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("looking up existing Stripe customer: %w", err)
	}
	if customerID == "" {
		logger.V(2).Info("no Stripe Customer recorded for BillingAccount yet; skipping push",
			"billingAccount", ba.Name)
		return ctrl.Result{}, nil
	}

	stripe, err := r.buildStripeClient(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	details, err := buildCustomerDetails(ctx, r.Client, &ba)
	if err != nil {
		return ctrl.Result{}, err
	}
	if _, err := stripe.EnsureCustomer(ctx, customerID, ba.Name, details); err != nil {
		// Tax-ID rejection means the rest of the Customer record did
		// sync; we'd just loop forever fighting a bad user-supplied
		// value if we returned an error. The StripePaymentMethod
		// reconciler is the surface where the user sees the tax-ID
		// failure condition, so log here and move on.
		var taxErr *stripeinternal.TaxIDError
		if errors.As(err, &taxErr) {
			logger.Info("BillingAccount sync hit a tax-ID failure on Stripe; rest of customer synced",
				"customerID", customerID, "billingAccount", ba.Name, "error", taxErr.Underlying)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("syncing Stripe customer %q from BillingAccount: %w", customerID, err)
	}
	logger.V(1).Info("synced Stripe Customer from BillingAccount",
		"customerID", customerID, "billingAccount", ba.Name)
	return ctrl.Result{}, nil
}

// buildStripeClient resolves the StripeProviderConfig and constructs
// the SDK wrapper. When a stripeClientFactory is installed (unit
// tests) we skip the resolve step entirely — the factory's whole
// purpose is to substitute a fake client without standing up a
// real config + secret in envtest.
func (r *BillingAccountReconciler) buildStripeClient(ctx context.Context) (stripeCustomerEnsurer, error) {
	if r.stripeClientFactory != nil {
		return r.stripeClientFactory(nil), nil
	}
	cfg, err := stripeinternal.ResolveConfig(ctx, r.Client, r.ProviderConfigName)
	if err != nil {
		return nil, fmt.Errorf("resolving StripeProviderConfig: %w", err)
	}
	return stripeinternal.NewClient(cfg), nil
}

// SetupWithManager wires the reconciler.
//
// The GenerationChangedPredicate filter restricts BillingAccount Update
// events to those that bump metadata.generation, which Kubernetes
// increments only when .spec changes. Without it the reconciler would
// fire on every status write from the billing controller, every
// finalizer add, and every label/annotation tweak — each one trailing
// a Stripe Customers.Update + TaxIDs reconcile loop. Create and Delete
// events still pass the predicate.
//
// Two secondary watches keep the Stripe Customer in sync with cluster
// state that isn't reflected on BillingAccount.spec:
//
//   - BillingAccountBinding — drives Customer.metadata.projects. A
//     new Active binding or a binding transitioning out of Active
//     needs to re-stamp the projects list on Stripe; without this
//     watch the list goes stale until the BA itself happens to be
//     edited.
//   - StripePaymentMethod — drives the default payment-method ID
//     mirrored onto Customer.invoice_settings.default_payment_method.
//     The pm_… ID lands on status.stripePaymentMethodId after the
//     SetupIntent confirms; without this watch a default chosen
//     before confirmation never propagates upstream.
//
// Both map their observed objects back to the parent BillingAccount
// and enqueue a reconcile against it.
func (r *BillingAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	return ctrl.NewControllerManagedBy(mgr).
		Named("billingaccount").
		For(&billingv1alpha1.BillingAccount{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&billingv1alpha1.BillingAccountBinding{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueBillingAccountForBinding),
		).
		Watches(
			&stripev1alpha1.StripePaymentMethod{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueBillingAccountForStripePaymentMethod),
		).
		Complete(r)
}

// enqueueBillingAccountForBinding maps a BillingAccountBinding event
// to a reconcile request for the BillingAccount the binding references.
// Same namespace by construction (BAB lives in the org namespace
// alongside the BA).
func (r *BillingAccountReconciler) enqueueBillingAccountForBinding(_ context.Context, obj client.Object) []reconcile.Request {
	bab, ok := obj.(*billingv1alpha1.BillingAccountBinding)
	if !ok {
		return nil
	}
	if bab.Spec.BillingAccountRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: bab.Namespace,
			Name:      bab.Spec.BillingAccountRef.Name,
		},
	}}
}

// enqueueBillingAccountForStripePaymentMethod maps a StripePaymentMethod
// event to a reconcile request for the BillingAccount that owns its
// parent PaymentMethod. The SPM's name matches its parent PM's name
// (paymentmethod-watcher invariant) so we can look the PM up directly
// without an extra owner-ref walk.
func (r *BillingAccountReconciler) enqueueBillingAccountForStripePaymentMethod(ctx context.Context, obj client.Object) []reconcile.Request {
	spm, ok := obj.(*stripev1alpha1.StripePaymentMethod)
	if !ok {
		return nil
	}
	pmName := spm.Spec.PaymentMethodRef.Name
	if pmName == "" {
		return nil
	}
	var pm billingv1alpha1.PaymentMethod
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: spm.Namespace, Name: pmName}, &pm); err != nil {
		// PaymentMethod gone (deleted out from under) or transient API
		// error — drop the event rather than enqueue a request the BA
		// reconciler can't satisfy. The next BA generation bump will
		// re-resolve everything anyway.
		return nil
	}
	if pm.Spec.BillingAccountRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: pm.Namespace,
			Name:      pm.Spec.BillingAccountRef.Name,
		},
	}}
}
