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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// BillingAccountReconciler keeps the Stripe Customer record in sync with
// the owning BillingAccount's spec, and creates it for accounts billed by
// invoice. The StripePaymentMethod reconciler creates the Customer when a
// card is added, but never re-runs against the same Customer when the
// BillingAccount changes later — so edits to `spec.contactInfo` (tax
// IDs, address, business name, invoice recipients) would otherwise never
// propagate to Stripe.
//
// An account needs a Stripe Customer once it has a StripePaymentMethod,
// or once billing reports it is billed by invoice (an Invoice
// BillingArrangement on status.paymentArrangement): invoicing providers
// bill the Customer published on its StripeCustomer, and an account with
// payment terms need never add a card. Accounts that need neither are a
// no-op, so abandoned sign-ups don't become Stripe Customers.
type BillingAccountReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme

	// ProviderConfigName names the StripeProviderConfig the reconciler
	// uses for SDK credentials. Set from the operator flag, same value
	// the StripePaymentMethodReconciler uses.
	ProviderConfigName string

	// Customers creates and syncs the account's Stripe Customer. Shared
	// with the StripePaymentMethodReconciler so the two can't create a
	// Customer each for the same account. SetupWithManager creates one
	// when unset.
	Customers *StripeCustomers

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
	EnsureCustomer(ctx context.Context, existingID string, account stripeinternal.BillingAccountKey, details stripeinternal.CustomerDetails) (string, error)
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripecustomers,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripecustomers/status,verbs=get;update;patch
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
		// The Stripe Customer outlives the account: it holds the
		// invoice history. The StripeCustomer is garbage collected with
		// the account. Nothing to do here.
		return ctrl.Result{}, nil
	}

	needed, err := r.needsStripeCustomer(ctx, &ba)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !needed {
		logger.V(2).Info("BillingAccount has no payment method and isn't billed by invoice; no Stripe Customer needed",
			"billingAccount", ba.Name)
		return ctrl.Result{}, nil
	}

	stripe, err := r.buildStripeClient(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	customerID, err := r.customers().Ensure(ctx, stripe, &ba)
	if err != nil {
		// Tax-ID rejection means the rest of the Customer record did
		// sync; we'd just loop forever fighting a bad user-supplied
		// value if we returned an error. The StripeCustomer's
		// TaxIDsApplied condition records the failure.
		var taxErr *stripeinternal.TaxIDError
		if errors.As(err, &taxErr) {
			logger.Info("BillingAccount sync hit a tax-ID failure on Stripe; rest of customer synced",
				"customerID", customerID, "billingAccount", ba.Name, "error", taxErr.Underlying)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("syncing Stripe customer from BillingAccount %q: %w", ba.Name, err)
	}
	logger.V(1).Info("synced Stripe Customer from BillingAccount",
		"customerID", customerID, "billingAccount", ba.Name)
	return ctrl.Result{}, nil
}

// needsStripeCustomer reports whether the account should have a Stripe
// Customer: it already has one (a StripeCustomer, or a
// StripePaymentMethod from before StripeCustomer existed), a payment
// method is being added, or it is billed by invoice.
func (r *BillingAccountReconciler) needsStripeCustomer(ctx context.Context, ba *billingv1alpha1.BillingAccount) (bool, error) {
	if hasInvoiceArrangement(ba) {
		return true, nil
	}
	var sc stripev1alpha1.StripeCustomer
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ba.Namespace, Name: ba.Name}, &sc)
	if err == nil {
		return true, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("getting StripeCustomer %q: %w", ba.Name, err)
	}
	spms, err := accountStripePaymentMethods(ctx, r.Client, ba.Namespace, ba.Name)
	if err != nil {
		return false, fmt.Errorf("listing StripePaymentMethods for BillingAccount %q: %w", ba.Name, err)
	}
	return len(spms) > 0, nil
}

// hasInvoiceArrangement reports whether billing bills the account by
// invoice: it publishes the active BillingArrangement on
// status.paymentArrangement and clears it when none is active.
func hasInvoiceArrangement(ba *billingv1alpha1.BillingAccount) bool {
	arr := ba.Status.PaymentArrangement
	return arr != nil && arr.Type == billingv1alpha1.BillingArrangementTypeInvoice
}

func (r *BillingAccountReconciler) customers() *StripeCustomers {
	if r.Customers == nil {
		r.Customers = NewStripeCustomers(r.Client, r.Client)
	}
	return r.Customers
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
// The BillingAccount predicate restricts Update events to those that
// bump metadata.generation, which Kubernetes increments only when .spec
// changes, or that start or end billing by invoice. Without it the
// reconciler would fire on every status write from the billing
// controller, every finalizer add, and every label/annotation tweak —
// each one trailing a Stripe Customers.Update + TaxIDs reconcile loop.
// Create and Delete events still pass the predicate.
//
// Three secondary watches keep the Stripe Customer in sync with cluster
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
//   - StripeCustomer — recreates one that was deleted while its
//     account still needs it. Status writes don't change its
//     generation, so the reconciler's own writes don't requeue it.
//
// The first two map their observed objects back to the parent
// BillingAccount and enqueue a reconcile against it.
func (r *BillingAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	if r.Customers == nil {
		r.Customers = NewStripeCustomers(mgr.GetClient(), mgr.GetAPIReader())
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("billingaccount").
		For(&billingv1alpha1.BillingAccount{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			invoiceArrangementChanged,
		))).
		Owns(&stripev1alpha1.StripeCustomer{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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

// invoiceArrangementChanged passes BillingAccount updates that start or
// end billing by invoice. billing reports that on status, which doesn't
// change the generation, and an account that starts paying by invoice
// needs a Stripe Customer.
var invoiceArrangementChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldBA, okOld := e.ObjectOld.(*billingv1alpha1.BillingAccount)
		newBA, okNew := e.ObjectNew.(*billingv1alpha1.BillingAccount)
		return okOld && okNew && hasInvoiceArrangement(oldBA) != hasInvoiceArrangement(newBA)
	},
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
