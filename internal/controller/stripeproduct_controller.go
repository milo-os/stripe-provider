// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// StripeNontaxableTaxCode is Stripe's tax code for a product that's never
// taxed. A pricing that isn't taxable is sold as it.
const StripeNontaxableTaxCode = "txcd_00000000"

// stripeProductEnsurer is the narrow surface of the Stripe SDK wrapper the
// StripeProduct reconciler needs, so tests can stand in for Stripe.
type stripeProductEnsurer interface {
	EnsureProduct(ctx context.Context, d stripeinternal.ProductDetails) error
}

// ServicePricingProductReconciler gives every ServicePricing a
// StripeProduct to stand for it: one per SKU, or one per list pricing
// without a SKU. It only creates them; the StripeProductReconciler does
// the rest.
type ServicePricingProductReconciler struct {
	Client client.Client
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=servicepricings,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproducts,verbs=get;list;watch;create

func (r *ServicePricingProductReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var sp billingv1alpha1.ServicePricing
	if err := r.Client.Get(ctx, req.NamespacedName, &sp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sp.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	product := &stripev1alpha1.StripeProduct{
		ObjectMeta: metav1.ObjectMeta{Name: stripeProductName(&sp), Namespace: sp.Namespace},
	}
	if sku := sp.Spec.SKU; sku != "" {
		product.Spec.SKU = sku
	} else {
		product.Spec.PricingRef = &stripev1alpha1.ServicePricingLocalRef{Name: listPricingName(&sp)}
	}
	if err := r.Client.Create(ctx, product); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("creating StripeProduct %s: %w", product.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler.
func (r *ServicePricingProductReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("servicepricing-product").
		For(&billingv1alpha1.ServicePricing{}).
		Complete(r)
}

// StripeProductReconciler keeps each StripeProduct's Stripe Product in
// step with the ServicePricings it stands for: named and taxed as the
// newest list pricing, and archived once none is left.
type StripeProductReconciler struct {
	Client client.Client

	// ProviderConfigName names the StripeProviderConfig for SDK
	// credentials, as for the other reconcilers.
	ProviderConfigName string

	// stripeClientFactory lets tests stand in for Stripe. Production
	// leaves it nil.
	stripeClientFactory func() stripeProductEnsurer
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=servicepricings,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproducts,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproducts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproviderconfigs,verbs=get;list;watch

func (r *StripeProductReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var product stripev1alpha1.StripeProduct
	if err := r.Client.Get(ctx, req.NamespacedName, &product); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !product.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	var pricings billingv1alpha1.ServicePricingList
	if err := r.Client.List(ctx, &pricings, client.InNamespace(product.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ServicePricings: %w", err)
	}
	desired := desiredProduct(&product, pricings.Items)

	stripe, err := r.buildStripeClient(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	details := stripeinternal.ProductDetails{
		ID:          stripeProductID(&product),
		Name:        desired.name,
		TaxCode:     desired.taxCode,
		Description: desired.description,
		Active:      desired.active,
		Metadata:    stripeinternal.ProductMetadata(product.Namespace, product.Name, product.Spec.SKU),
	}
	if details.Name == "" {
		// Nothing has ever stood for it, so there's nothing to name it.
		logger.V(1).Info("no ServicePricing stands for StripeProduct; nothing to sync", "stripeProduct", product.Name)
		return ctrl.Result{}, nil
	}
	syncErr := stripe.EnsureProduct(ctx, details)

	before := product.Status.DeepCopy()
	product.Status.ObservedGeneration = product.Generation
	if syncErr == nil {
		product.Status.ProductID = details.ID
		product.Status.Name = details.Name
		product.Status.TaxCode = details.TaxCode
		product.Status.Active = details.Active
		product.Status.Pricings = desired.pricings
		apimeta.SetStatusCondition(&product.Status.Conditions, metav1.Condition{
			Type: stripev1alpha1.StripeProductConditionReady, Status: metav1.ConditionTrue,
			Reason: "Synced", Message: "The Stripe product matches its pricings",
			ObservedGeneration: product.Generation,
		})
	} else {
		apimeta.SetStatusCondition(&product.Status.Conditions, metav1.Condition{
			Type: stripev1alpha1.StripeProductConditionReady, Status: metav1.ConditionFalse,
			Reason: "StripeError", Message: syncErr.Error(),
			ObservedGeneration: product.Generation,
		})
	}
	conflict := metav1.Condition{
		Type: stripev1alpha1.StripeProductConditionTaxCodeConflict, Status: metav1.ConditionFalse,
		Reason: "Consistent", Message: "Every list pricing is taxed the same way",
		ObservedGeneration: product.Generation,
	}
	if len(desired.conflicts) > 0 {
		conflict.Status = metav1.ConditionTrue
		conflict.Reason = "ListPricingsDiffer"
		conflict.Message = fmt.Sprintf("Taxed as %s, the newest list pricing; %s taxed differently",
			taxCodeLabel(desired.taxCode), strings.Join(desired.conflicts, ", "))
	}
	apimeta.SetStatusCondition(&product.Status.Conditions, conflict)

	if !equalProductStatus(before, &product.Status) {
		if err := r.Client.Status().Update(ctx, &product); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating StripeProduct status: %w", err)
		}
	}
	return ctrl.Result{}, syncErr
}

func (r *StripeProductReconciler) buildStripeClient(ctx context.Context) (stripeProductEnsurer, error) {
	if r.stripeClientFactory != nil {
		return r.stripeClientFactory(), nil
	}
	cfg, err := stripeinternal.ResolveConfig(ctx, r.Client, r.ProviderConfigName)
	if err != nil {
		return nil, fmt.Errorf("resolving StripeProviderConfig: %w", err)
	}
	return stripeinternal.NewClient(cfg), nil
}

// SetupWithManager wires the reconciler. A ServicePricing event requeues
// the StripeProduct that stands for it.
func (r *StripeProductReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("stripeproduct").
		For(&stripev1alpha1.StripeProduct{}).
		Watches(&billingv1alpha1.ServicePricing{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				sp, ok := obj.(*billingv1alpha1.ServicePricing)
				if !ok {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: sp.Namespace, Name: stripeProductName(sp),
				}}}
			})).
		Complete(r)
}

// productState is what a StripeProduct's Product should be.
type productState struct {
	name    string
	taxCode string
	// description is the newest pricing's, nil when no pricing stands for
	// the Product, which then keeps its own.
	description *string
	active      bool
	pricings    []string
	// conflicts are the list pricings taxed differently from the newest.
	conflicts []string
}

// desiredProduct works out the Product from the pricings in its
// namespace. The newest list pricing names and taxes it; with no list
// pricing left, the newest customer pricing does. With none at all, the
// Product keeps its last name and tax code and is archived.
func desiredProduct(product *stripev1alpha1.StripeProduct, all []billingv1alpha1.ServicePricing) productState {
	var members []billingv1alpha1.ServicePricing
	for i := range all {
		sp := &all[i]
		if sp.DeletionTimestamp.IsZero() && stripeProductName(sp) == product.Name {
			members = append(members, *sp)
		}
	}
	if len(members) == 0 {
		return productState{name: product.Status.Name, taxCode: product.Status.TaxCode}
	}

	state := productState{active: true}
	for _, sp := range members {
		state.pricings = append(state.pricings, sp.Name)
	}
	slices.Sort(state.pricings)

	lists := slices.DeleteFunc(slices.Clone(members), func(sp billingv1alpha1.ServicePricing) bool {
		return sp.Spec.BasedOn != ""
	})
	if len(lists) == 0 {
		lists = members
	}
	newest := slices.MaxFunc(lists, func(a, b billingv1alpha1.ServicePricing) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	state.name = newest.Spec.DisplayName
	if state.name == "" {
		state.name = newest.Name
	}
	state.taxCode = pricingTaxCode(&newest)
	description := newest.Spec.Description
	state.description = &description
	for _, sp := range lists {
		if pricingTaxCode(&sp) != state.taxCode {
			state.conflicts = append(state.conflicts, sp.Name)
		}
	}
	slices.Sort(state.conflicts)
	return state
}

// pricingTaxCode is the Stripe tax code a pricing's charges are taxed by:
// its own code, Stripe's nontaxable code when it isn't taxable, or "" for
// the account's default.
func pricingTaxCode(sp *billingv1alpha1.ServicePricing) string {
	tax := sp.Spec.Tax
	switch {
	case !tax.IsTaxable():
		return StripeNontaxableTaxCode
	case tax != nil:
		return tax.Code
	}
	return ""
}

func taxCodeLabel(code string) string {
	if code == "" {
		return "the account's default"
	}
	return code
}

// listPricingName is the list pricing a pricing stands with: the one its
// basedOn names, or itself.
func listPricingName(sp *billingv1alpha1.ServicePricing) string {
	if sp.Spec.BasedOn != "" {
		return sp.Spec.BasedOn
	}
	return sp.Name
}

// stripeProductName is the name of the StripeProduct that stands for a
// pricing: one per SKU, or one per list pricing without a SKU. A customer
// pricing has its list pricing's SKU, so either way it shares the list
// pricing's Product.
func stripeProductName(sp *billingv1alpha1.ServicePricing) string {
	if sku := sp.Spec.SKU; sku != "" {
		// SKUs are case-sensitive and may hold "_", which names can't, so a
		// hash of the exact SKU keeps two SKUs from sharing a name.
		return boundedName("sku-"+dnsLabel(sku), sku)
	}
	return boundedName("pricing-"+listPricingName(sp), "")
}

// dnsLabel lower-cases s and turns anything a Kubernetes name can't hold
// into "-".
func dnsLabel(s string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			b[i] = '-'
		}
	}
	return strings.Trim(string(b), "-.")
}

// maxNameLength is the longest Kubernetes object name.
const maxNameLength = 253

// boundedName is base, suffixed with a short hash of hashOf when that's
// set, and cut to fit a Kubernetes name with a hash of base when it's too
// long.
func boundedName(base, hashOf string) string {
	if hashOf != "" {
		base += "-" + shortHash(hashOf)
	}
	if len(base) <= maxNameLength {
		return base
	}
	suffix := "-" + shortHash(base)
	return strings.TrimRight(base[:maxNameLength-len(suffix)], "-.") + suffix
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
}

// stripeProductID is the Stripe Product ID for a StripeProduct. It's
// derived from the StripeProduct's namespace and name, so a lost create is
// found again rather than repeated.
func stripeProductID(p *stripev1alpha1.StripeProduct) string {
	sum := sha256.Sum256([]byte(p.Namespace + "/" + p.Name))
	return "datum_" + hex.EncodeToString(sum[:])[:24]
}

func equalProductStatus(a, b *stripev1alpha1.StripeProductStatus) bool {
	if a.ProductID != b.ProductID || a.Name != b.Name || a.TaxCode != b.TaxCode ||
		a.Active != b.Active || a.ObservedGeneration != b.ObservedGeneration ||
		!slices.Equal(a.Pricings, b.Pricings) || len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for _, c := range a.Conditions {
		other := apimeta.FindStatusCondition(b.Conditions, c.Type)
		if other == nil || other.Status != c.Status || other.Reason != c.Reason || other.Message != c.Message ||
			other.ObservedGeneration != c.ObservedGeneration {
			return false
		}
	}
	return true
}
