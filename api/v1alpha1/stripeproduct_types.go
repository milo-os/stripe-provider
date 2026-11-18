// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// StripeProductConditionReady is True once the Stripe Product exists
	// and matches the pricings it stands for.
	StripeProductConditionReady = "Ready"
	// StripeProductConditionTaxCodeConflict is True when list pricings with
	// the product's SKU are taxed differently. The newest one's tax code is
	// used; the condition names the others, for finance to look at.
	StripeProductConditionTaxCodeConflict = "TaxCodeConflict"
)

// StripeProductSpec defines the desired state of a StripeProduct.
//
// A StripeProduct stands for every ServicePricing with its SKU, or for
// one pricing without a SKU and the customer pricings based on it. Exactly
// one of sku and pricingRef is set.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.sku) != has(self.pricingRef)",message="exactly one of sku and pricingRef is set"
type StripeProductSpec struct {
	// SKU is the product code of the ServicePricings this Product stands
	// for, exactly as they spell it.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	SKU string `json:"sku,omitempty"`

	// PricingRef names the one list ServicePricing, in this namespace, that
	// this Product stands for when it has no SKU. Customer pricings based on
	// it share the Product.
	//
	// +kubebuilder:validation:Optional
	PricingRef *ServicePricingLocalRef `json:"pricingRef,omitempty"`
}

// ServicePricingLocalRef is an in-namespace reference to a billing
// ServicePricing.
type ServicePricingLocalRef struct {
	// Name is the name of the ServicePricing.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// StripeProductStatus defines the observed state of a StripeProduct.
type StripeProductStatus struct {
	// ProductID is the Stripe Product ID. Invoice lines for the
	// pricings this Product stands for name it.
	//
	// +kubebuilder:validation:Optional
	ProductID string `json:"productId,omitempty"`

	// Name is the Product's name in Stripe: the display name of the newest
	// list pricing, or its name when it has no display name.
	//
	// +kubebuilder:validation:Optional
	Name string `json:"name,omitempty"`

	// TaxCode is the Product's Stripe tax code, from the newest list
	// pricing. Empty when it has none, so the account's default applies.
	//
	// +kubebuilder:validation:Optional
	TaxCode string `json:"taxCode,omitempty"`

	// Active is false once no ServicePricing stands for the Product. Stripe
	// keeps an invoiced Product, so it's archived rather than deleted, and
	// made active again if a pricing comes back.
	//
	// +kubebuilder:validation:Optional
	Active bool `json:"active,omitempty"`

	// Pricings are the ServicePricings the Product stands for, list
	// pricings and customer pricings alike, sorted by name.
	//
	// +kubebuilder:validation:Optional
	// +listType=set
	Pricings []string `json:"pricings,omitempty"`

	// Conditions represent the latest available observations.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// StripeProduct is the Schema for the stripeproducts API.
//
// StripeProduct keeps a Stripe Product in step with the ServicePricings of
// one SKU, so invoice lines can name what was sold. The stripe-provider
// creates one in the pricings' namespace for each SKU, and one for each
// list pricing without a SKU. Nobody else writes them. A price changing
// never changes the Product: lines carry their own unit price.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="SKU",type=string,JSONPath=`.spec.sku`
// +kubebuilder:printcolumn:name="Product",type=string,JSONPath=`.status.productId`
// +kubebuilder:printcolumn:name="Tax code",type=string,JSONPath=`.status.taxCode`
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type StripeProduct struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StripeProductSpec   `json:"spec,omitempty"`
	Status StripeProductStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StripeProductList contains a list of StripeProduct.
type StripeProductList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StripeProduct `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StripeProduct{}, &StripeProductList{})
}
