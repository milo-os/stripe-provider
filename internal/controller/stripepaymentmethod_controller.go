// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// StripePaymentMethodReconciler drives the Stripe-side lifecycle: it
// ensures a Stripe Customer for the owning billing account, creates a
// SetupIntent, and writes clientSecret + status to the
// StripePaymentMethod. Webhook events from Stripe (handled in
// internal/webhook) move the resource into Active or Failed; once Active
// the controller projects the AwaitingConfirmation -> Active phase
// transition onto the parent PaymentMethod.
type StripePaymentMethodReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme

	// ProviderConfigName names the StripeProviderConfig the reconciler
	// uses for SDK credentials. Today only a single provider config is
	// supported per cluster; this is set from the operator flag.
	ProviderConfigName string
}

// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *StripePaymentMethodReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var spm stripev1alpha1.StripePaymentMethod
	if err := r.Client.Get(ctx, req.NamespacedName, &spm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !spm.DeletionTimestamp.IsZero() {
		// TODO: finalizer + Stripe-side detach. Out of scope for the
		// initial implementation.
		return ctrl.Result{}, nil
	}

	// Terminal states are sticky.
	switch spm.Status.Phase {
	case stripev1alpha1.StripePaymentMethodPhaseActive, stripev1alpha1.StripePaymentMethodPhaseFailed:
		return ctrl.Result{}, nil
	}

	// Once we have an in-flight SetupIntent we wait for the webhook to
	// move us forward.
	if spm.Status.SetupIntent != nil && spm.Status.SetupIntent.ClientSecret != "" {
		return ctrl.Result{}, r.ensurePaymentMethodAwaiting(ctx, &spm)
	}

	// Look up the parent PaymentMethod (and through it the BillingAccount).
	var pm billingv1alpha1.PaymentMethod
	pmKey := types.NamespacedName{Namespace: spm.Namespace, Name: spm.Spec.PaymentMethodRef.Name}
	if err := r.Client.Get(ctx, pmKey, &pm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting parent PaymentMethod %s/%s: %w", pmKey.Namespace, pmKey.Name, err)
	}
	var ba billingv1alpha1.BillingAccount
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: pm.Spec.BillingAccountRef.Name}, &ba); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting BillingAccount %q: %w", pm.Spec.BillingAccountRef.Name, err)
	}

	cfg, err := stripeinternal.ResolveConfig(ctx, r.Client, r.ProviderConfigName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving StripeProviderConfig: %w", err)
	}
	stripe := stripeinternal.NewClient(cfg)

	email := ""
	if ba.Spec.ContactInfo != nil {
		email = ba.Spec.ContactInfo.Email
	}
	customerID, err := stripe.EnsureCustomer(ctx, spm.Status.StripeCustomerID, ba.Name, email)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Stripe customer: %w", err)
	}
	si, err := stripe.CreateSetupIntent(ctx, customerID, spm.Namespace, spm.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating Stripe SetupIntent: %w", err)
	}

	base := spm.DeepCopy()
	spm.Status.Phase = stripev1alpha1.StripePaymentMethodPhaseAwaitingConfirmation
	spm.Status.StripeCustomerID = customerID
	spm.Status.SetupIntent = &stripev1alpha1.StripeSetupIntentStatus{
		ID:           si.ID,
		ClientSecret: si.ClientSecret,
		Status:       si.Status,
	}
	apimeta.SetStatusCondition(&spm.Status.Conditions, metav1.Condition{
		Type:               "SetupIntentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: spm.Generation,
		Reason:             "ClientSecretAvailable",
		Message:            fmt.Sprintf("Stripe SetupIntent %s created.", si.ID),
	})
	spm.Status.ObservedGeneration = spm.Generation
	if err := r.Client.Status().Patch(ctx, &spm, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching StripePaymentMethod status: %w", err)
	}
	logger.Info("created Stripe SetupIntent",
		"setupIntent", si.ID, "customer", customerID, "stripePaymentMethod", spm.Name)

	if err := r.ensurePaymentMethodAwaiting(ctx, &spm); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ensurePaymentMethodAwaiting moves the parent PaymentMethod phase to
// AwaitingConfirmation once the Stripe side has a client secret ready.
// Idempotent: if the parent already reports Awaiting/Active/Failed it
// leaves it alone.
func (r *StripePaymentMethodReconciler) ensurePaymentMethodAwaiting(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod) error {
	var pm billingv1alpha1.PaymentMethod
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: spm.Namespace, Name: spm.Spec.PaymentMethodRef.Name}, &pm); err != nil {
		return fmt.Errorf("getting parent PaymentMethod: %w", err)
	}
	if pm.Status.Phase == billingv1alpha1.PaymentMethodPhaseAwaitingConfirmation ||
		pm.Status.Phase == billingv1alpha1.PaymentMethodPhaseActive ||
		pm.Status.Phase == billingv1alpha1.PaymentMethodPhaseFailed {
		return nil
	}
	base := pm.DeepCopy()
	pm.Status.Phase = billingv1alpha1.PaymentMethodPhaseAwaitingConfirmation
	apimeta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.PaymentMethodConditionInstrumentReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: pm.Generation,
		Reason:             "AwaitingConfirmation",
		Message:            "Stripe SetupIntent created; awaiting consumer confirmation.",
	})
	return r.Client.Status().Patch(ctx, &pm, client.MergeFrom(base))
}

// SetupWithManager wires the reconciler.
func (r *StripePaymentMethodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	return ctrl.NewControllerManagedBy(mgr).
		Named("stripepaymentmethod").
		For(&stripev1alpha1.StripePaymentMethod{}).
		Complete(r)
}
