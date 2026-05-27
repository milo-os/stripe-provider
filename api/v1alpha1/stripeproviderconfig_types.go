// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StripeProviderConfigSpec defines the desired state of a
// StripeProviderConfig. A StripeProviderConfig is referenced by a
// PaymentMethodClass via parametersRef and carries the Stripe SDK
// configuration the provider needs to operate.
type StripeProviderConfigSpec struct {
	// PublishableKey is the Stripe publishable API key used by the portal
	// to initialize Stripe.js. This value is non-sensitive and is read by
	// the portal directly via the parametersRef chain on
	// PaymentMethodClass.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PublishableKey string `json:"publishableKey"`

	// APIVersion pins the Stripe API version used for outbound requests.
	// When unset, the SDK default is used.
	//
	// Stripe API credentials (secret key + webhook signing secret) are
	// NOT carried on this CRD: surfacing SecretKeySelectors on a
	// cluster-scoped, user-facing API leaks the secret-name vocabulary
	// across tenants and complicates the IAM model. Deployment
	// operators wire those secrets in via environment variables
	// (STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET) set on the controller
	// Pod — typically sourced from a Kubernetes Secret synced out of
	// an external secret manager.
	//
	// +kubebuilder:validation:Optional
	APIVersion string `json:"apiVersion,omitempty"`
}

// StripeProviderConfigStatus defines the observed state of a StripeProviderConfig.
type StripeProviderConfigStatus struct {
	// Conditions represent the latest available observations of the
	// config's state.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// SupportedTaxIDTypes is the list of tax-ID types this Stripe
	// integration can accept on a Customer.tax_ids entry. Published
	// by the stripe-provider controller from the Stripe SDK's
	// tax_id_data.type vocabulary plus a curated metadata table for
	// display names, example formats, and the country the type
	// belongs to.
	//
	// Consumers (the portal in particular) read this list to drive
	// the Tax ID type dropdown on the billing-details form without
	// hardcoding the Stripe vocabulary into the front-end.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	SupportedTaxIDTypes []SupportedTaxIDType `json:"supportedTaxIDTypes,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// SupportedTaxIDType describes a single tax-ID type a Stripe-backed
// PaymentMethodClass can accept.
type SupportedTaxIDType struct {
	// Type is the vendor-neutral tax-ID type code, matching the value
	// stored on BillingAccount.spec.taxIds[].type (e.g. "gb_vat",
	// "eu_vat", "us_ein"). For Stripe this also happens to match
	// Stripe's own tax_id_data.type vocabulary.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z]{2}_[a-z][a-z_]*$`
	Type string `json:"type"`

	// DisplayName is the human-readable label shown in the portal
	// dropdown (e.g. "United Kingdom VAT").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Example is a sample registration number used as placeholder
	// text in the portal input (e.g. "GB123456789"). Empty when the
	// metadata table has no curated example for this type.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	Example string `json:"example,omitempty"`

	// Country is the ISO 3166-1 alpha-2 country code the type is
	// registered against, or "EU" for cross-jurisdictional types
	// (e.g. EU VAT). Empty when the metadata table has no curated
	// country mapping for this type.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=2
	Country string `json:"country,omitempty"`
}

// StripeProviderConfig is the Schema for the stripeproviderconfigs API.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type StripeProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StripeProviderConfigSpec   `json:"spec,omitempty"`
	Status StripeProviderConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StripeProviderConfigList contains a list of StripeProviderConfig.
type StripeProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StripeProviderConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StripeProviderConfig{}, &StripeProviderConfigList{})
}
