// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	"go.miloapis.com/stripe-provider/internal/taxids"
)

// StripeProviderConfigReconciler publishes the curated tax-ID metadata
// onto StripeProviderConfig.status.supportedTaxIDTypes. The portal
// reads this list to drive the Tax ID type dropdown on the billing-
// details form so the front-end never has to hardcode Stripe's
// tax_id_data vocabulary.
//
// The metadata source is internal/taxids — a curated table that
// mirrors stripe-go v81's TaxIDType vocabulary 1:1. When stripe-go is
// bumped and the table is updated, the next reconcile republishes
// status for every StripeProviderConfig in the cluster.
type StripeProviderConfigReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproviderconfigs/status,verbs=get;update;patch

func (r *StripeProviderConfigReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cfg stripev1alpha1.StripeProviderConfig
	if err := r.Client.Get(ctx, req.NamespacedName, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	desired := desiredSupportedTaxIDTypes()

	if reflect.DeepEqual(cfg.Status.SupportedTaxIDTypes, desired) &&
		cfg.Status.ObservedGeneration == cfg.Generation &&
		apimeta.IsStatusConditionTrue(cfg.Status.Conditions, "Ready") {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(cfg.DeepCopy())
	cfg.Status.SupportedTaxIDTypes = desired
	cfg.Status.ObservedGeneration = cfg.Generation
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cfg.Generation,
		Reason:             "Published",
		Message:            "Tax-ID metadata published to status.supportedTaxIDTypes.",
	})
	if err := r.Client.Status().Patch(ctx, &cfg, patch); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("published supportedTaxIDTypes", "name", cfg.Name, "count", len(desired))
	return ctrl.Result{}, nil
}

func desiredSupportedTaxIDTypes() []stripev1alpha1.SupportedTaxIDType {
	src := taxids.All()
	out := make([]stripev1alpha1.SupportedTaxIDType, len(src))
	for i, e := range src {
		out[i] = stripev1alpha1.SupportedTaxIDType{
			Type:        e.Type,
			DisplayName: e.DisplayName,
			Example:     e.Example,
			Country:     e.Country,
		}
	}
	return out
}

// SetupWithManager wires the reconciler.
func (r *StripeProviderConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	return ctrl.NewControllerManagedBy(mgr).
		Named("stripeproviderconfig").
		For(&stripev1alpha1.StripeProviderConfig{}).
		Complete(r)
}
