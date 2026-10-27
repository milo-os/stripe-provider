// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// StripeCustomerConditionReady is True once the Stripe customer exists
	// and status.stripeCustomerId names it.
	StripeCustomerConditionReady = "Ready"

	// StripeCustomerConditionTaxIDsApplied reports whether Stripe accepted
	// the billing account's tax IDs. A rejected tax ID does not stop the
	// customer from being Ready.
	StripeCustomerConditionTaxIDsApplied = "TaxIDsApplied"
)

// StripeCustomerSpec defines the desired state of a StripeCustomer.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type StripeCustomerSpec struct {
	// BillingAccountRef names the billing.miloapis.com BillingAccount in
	// the same namespace that this Stripe customer pays for.
	//
	// +kubebuilder:validation:Required
	BillingAccountRef BillingAccountLocalRef `json:"billingAccountRef"`
}

// BillingAccountLocalRef is an in-namespace reference to a billing
// BillingAccount.
type BillingAccountLocalRef struct {
	// Name is the name of the BillingAccount.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// StripeCustomerStatus defines the observed state of a StripeCustomer.
type StripeCustomerStatus struct {
	// StripeCustomerID is the Stripe customer ID (`cus_…`) for the billing
	// account. Every StripePaymentMethod for the account uses this
	// customer, and invoicing providers bill it.
	//
	// +kubebuilder:validation:Optional
	StripeCustomerID string `json:"stripeCustomerId,omitempty"`

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

// StripeCustomer is the Schema for the stripecustomers API.
//
// StripeCustomer publishes the Stripe customer for a billing
// BillingAccount, independent of any payment method. The stripe-provider
// creates one, named after the BillingAccount and owned by it, as soon
// as the account needs a Stripe customer: when a payment method is added
// or when the account is billed by invoice. Invoicing providers read
// status.stripeCustomerId from here, so an account with payment terms
// can be invoiced without ever adding a card.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="BillingAccount",type=string,JSONPath=`.spec.billingAccountRef.name`
// +kubebuilder:printcolumn:name="Customer",type=string,JSONPath=`.status.stripeCustomerId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type StripeCustomer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StripeCustomerSpec   `json:"spec,omitempty"`
	Status StripeCustomerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StripeCustomerList contains a list of StripeCustomer.
type StripeCustomerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StripeCustomer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StripeCustomer{}, &StripeCustomerList{})
}
