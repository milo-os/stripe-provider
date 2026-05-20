// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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

	// SecretKeyRef references a Secret key holding the Stripe secret API
	// key used for server-to-server calls. Required for the provider to
	// create customers and SetupIntents.
	//
	// +kubebuilder:validation:Required
	SecretKeyRef corev1.SecretKeySelector `json:"secretKeyRef"`

	// WebhookSecretRef references a Secret key holding the signing secret
	// used to verify Stripe webhook payloads.
	//
	// +kubebuilder:validation:Required
	WebhookSecretRef corev1.SecretKeySelector `json:"webhookSecretRef"`

	// APIVersion pins the Stripe API version used for outbound requests.
	// When unset, the SDK default is used.
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

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
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
