// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// PaymentMethodWatcher watches billing PaymentMethod resources and
// creates a StripePaymentMethod child whenever a PaymentMethod selects a
// Stripe-backed PaymentMethodClass.
//
// The watcher only handles the "did the parent get created?" side of the
// relationship. StripePaymentMethod itself is reconciled by
// StripePaymentMethodReconciler.
type PaymentMethodWatcher struct {
	Client client.Client
	Scheme *runtime.Scheme

	// ProviderName is the spec.provider value the watcher claims (e.g.
	// "stripe"). Only PaymentMethods whose class spec.provider matches
	// this value are reconciled.
	ProviderName string
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethodclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods,verbs=get;list;watch;create;update;patch;delete

func (r *PaymentMethodWatcher) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pm billingv1alpha1.PaymentMethod
	if err := r.Client.Get(ctx, req.NamespacedName, &pm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !pm.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if pm.Spec.PaymentMethodClassRef == nil || pm.Spec.PaymentMethodClassRef.Name == "" {
		return ctrl.Result{}, nil
	}

	// Check that this watcher owns the class.
	var pmc billingv1alpha1.PaymentMethodClass
	if err := r.Client.Get(ctx, types.NamespacedName{Name: pm.Spec.PaymentMethodClassRef.Name}, &pmc); err != nil {
		if apierrors.IsNotFound(err) {
			// Defaulting webhook prevented this, but tolerate it.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting PaymentMethodClass %q: %w", pm.Spec.PaymentMethodClassRef.Name, err)
	}
	if pmc.Spec.Provider != r.ProviderName {
		return ctrl.Result{}, nil
	}

	// Ensure a StripePaymentMethod child exists.
	var existing stripev1alpha1.StripePaymentMethod
	childKey := types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}
	switch err := r.Client.Get(ctx, childKey, &existing); {
	case err == nil:
		return ctrl.Result{}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("getting StripePaymentMethod %s/%s: %w", childKey.Namespace, childKey.Name, err)
	}

	child := &stripev1alpha1.StripePaymentMethod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pm.Name,
			Namespace: pm.Namespace,
		},
		Spec: stripev1alpha1.StripePaymentMethodSpec{
			PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: pm.Name},
		},
	}
	if err := controllerutil.SetControllerReference(&pm, child, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference: %w", err)
	}
	if err := r.Client.Create(ctx, child); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating StripePaymentMethod %s/%s: %w", child.Namespace, child.Name, err)
	}
	logger.Info("created StripePaymentMethod for PaymentMethod",
		"namespace", pm.Namespace, "paymentMethod", pm.Name, "class", pmc.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager wires the watcher.
func (r *PaymentMethodWatcher) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	return ctrl.NewControllerManagedBy(mgr).
		Named("stripe-paymentmethod-watcher").
		For(&billingv1alpha1.PaymentMethod{}).
		Owns(&stripev1alpha1.StripePaymentMethod{}).
		Complete(r)
}
