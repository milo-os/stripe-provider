// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StripePaymentMethodPhase represents the lifecycle state of a StripePaymentMethod.
// +kubebuilder:validation:Enum=Initializing;AwaitingConfirmation;Active;Failed
type StripePaymentMethodPhase string

const (
	StripePaymentMethodPhaseInitializing         StripePaymentMethodPhase = "Initializing"
	StripePaymentMethodPhaseAwaitingConfirmation StripePaymentMethodPhase = "AwaitingConfirmation"
	StripePaymentMethodPhaseActive               StripePaymentMethodPhase = "Active"
	StripePaymentMethodPhaseFailed               StripePaymentMethodPhase = "Failed"
)

// StripePaymentMethodInstrumentType identifies the broad category of
// confirmed payment instrument. Mirrors the billing PaymentMethod
// instrument-type vocabulary so projection between the two is direct.
// +kubebuilder:validation:Enum=card;usBankAccount
type StripePaymentMethodInstrumentType string

const (
	StripePaymentMethodInstrumentTypeCard          StripePaymentMethodInstrumentType = "card"
	StripePaymentMethodInstrumentTypeUSBankAccount StripePaymentMethodInstrumentType = "usBankAccount"
)

// StripePaymentMethodSpec defines the desired state of a StripePaymentMethod.
type StripePaymentMethodSpec struct {
	// PaymentMethodRef is the name of the parent
	// billing.miloapis.com PaymentMethod in the same namespace. The
	// stripe-provider always sets this when creating the resource as an
	// owner-referenced child of the PaymentMethod.
	//
	// +kubebuilder:validation:Required
	PaymentMethodRef PaymentMethodLocalRef `json:"paymentMethodRef"`
}

// PaymentMethodLocalRef is an in-namespace reference to a billing
// PaymentMethod.
type PaymentMethodLocalRef struct {
	// Name is the name of the PaymentMethod.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// StripeSetupIntentStatus captures the in-flight SetupIntent state.
type StripeSetupIntentStatus struct {
	// ID is the Stripe SetupIntent identifier (e.g. `seti_…`).
	//
	// +kubebuilder:validation:Optional
	ID string `json:"id,omitempty"`

	// ClientSecret is the single-use secret consumed by the portal to
	// initialize Stripe Elements. Treated as sensitive; least-privilege
	// access on this CRD is the access boundary.
	//
	// +kubebuilder:validation:Optional
	ClientSecret string `json:"clientSecret,omitempty"`

	// Status is the most recent upstream SetupIntent status string
	// (`requires_payment_method`, `requires_action`, `succeeded`, ...).
	//
	// +kubebuilder:validation:Optional
	Status string `json:"status,omitempty"`

	// ExpiresAt is the expiry of the SetupIntent client secret. The
	// controller creates a fresh SetupIntent when this elapses without
	// confirmation.
	//
	// +kubebuilder:validation:Optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// StripeInstrumentDetails captures Stripe-side instrument metadata
// retrieved after confirmation. The billing-side projection onto
// PaymentMethod.status.details is derived from these fields.
type StripeInstrumentDetails struct {
	// Type identifies the instrument category.
	//
	// +kubebuilder:validation:Optional
	Type StripePaymentMethodInstrumentType `json:"type,omitempty"`

	// Card is populated when Type is card.
	//
	// +kubebuilder:validation:Optional
	Card *StripeCardInstrumentDetails `json:"card,omitempty"`

	// USBankAccount is populated when Type is usBankAccount.
	//
	// +kubebuilder:validation:Optional
	USBankAccount *StripeUSBankAccountInstrumentDetails `json:"usBankAccount,omitempty"`
}

// StripeCardInstrumentDetails carries Stripe-side card metadata.
type StripeCardInstrumentDetails struct {
	Brand                      string `json:"brand,omitempty"`
	Last4                      string `json:"last4,omitempty"`
	IssuerIdentificationNumber string `json:"issuerIdentificationNumber,omitempty"`
	Country                    string `json:"country,omitempty"`
	ExpiryMonth                int32  `json:"expiryMonth,omitempty"`
	ExpiryYear                 int32  `json:"expiryYear,omitempty"`
	AVSResult                  string `json:"avsResult,omitempty"`
	CVCResult                  string `json:"cvcResult,omitempty"`
}

// StripeUSBankAccountInstrumentDetails carries Stripe-side bank account metadata.
type StripeUSBankAccountInstrumentDetails struct {
	BankName    string `json:"bankName,omitempty"`
	Last4       string `json:"last4,omitempty"`
	AccountType string `json:"accountType,omitempty"`
}

// StripePaymentMethodStatus defines the observed state of a StripePaymentMethod.
type StripePaymentMethodStatus struct {
	// Phase represents the current lifecycle phase.
	//
	// +kubebuilder:validation:Optional
	Phase StripePaymentMethodPhase `json:"phase,omitempty"`

	// StripeCustomerID is the Stripe customer ID for the owning billing
	// account. Reused across all StripePaymentMethods for the same
	// account.
	//
	// +kubebuilder:validation:Optional
	StripeCustomerID string `json:"stripeCustomerId,omitempty"`

	// StripePaymentMethodID is the confirmed Stripe payment method ID
	// (`pm_…`). Populated when the SetupIntent succeeds.
	//
	// +kubebuilder:validation:Optional
	StripePaymentMethodID string `json:"stripePaymentMethodId,omitempty"`

	// SetupIntent captures the in-flight setup session.
	//
	// +kubebuilder:validation:Optional
	SetupIntent *StripeSetupIntentStatus `json:"setupIntent,omitempty"`

	// ConfirmedAt is the timestamp Stripe confirmed the SetupIntent.
	//
	// +kubebuilder:validation:Optional
	ConfirmedAt *metav1.Time `json:"confirmedAt,omitempty"`

	// Instrument carries Stripe-side metadata for the confirmed payment
	// instrument. The provider projects a normalized subset onto
	// billing PaymentMethod.status.details.
	//
	// +kubebuilder:validation:Optional
	Instrument *StripeInstrumentDetails `json:"instrument,omitempty"`

	// FailureReason / FailureMessage are populated when phase is Failed.
	//
	// +kubebuilder:validation:Optional
	FailureReason string `json:"failureReason,omitempty"`
	// +kubebuilder:validation:Optional
	FailureMessage string `json:"failureMessage,omitempty"`

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

// StripePaymentMethod is the Schema for the stripepaymentmethods API.
//
// StripePaymentMethod is a provider-owned child resource of a billing
// PaymentMethod. The stripe-provider creates it with an ownerReference
// pointing at the parent PaymentMethod so Kubernetes garbage collection
// cascades deletion. All Stripe-specific identifiers — customer ID,
// SetupIntent client secret, payment-method ID — live here exclusively.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="PaymentMethod",type=string,JSONPath=`.spec.paymentMethodRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="SetupIntent",type=string,JSONPath=`.status.setupIntent.id`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type StripePaymentMethod struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StripePaymentMethodSpec   `json:"spec,omitempty"`
	Status StripePaymentMethodStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StripePaymentMethodList contains a list of StripePaymentMethod.
type StripePaymentMethodList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StripePaymentMethod `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StripePaymentMethod{}, &StripePaymentMethodList{})
}
