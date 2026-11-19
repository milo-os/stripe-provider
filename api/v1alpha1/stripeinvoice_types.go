// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StripeInvoicePurpose says what a StripeInvoice is for.
//
// +kubebuilder:validation:Enum=Preview;PreviewDraft
type StripeInvoicePurpose string

const (
	// StripeInvoicePurposePreview asks Stripe what the invoice would be,
	// tax included, without creating anything in Stripe.
	StripeInvoicePurposePreview StripeInvoicePurpose = "Preview"
	// StripeInvoicePurposePreviewDraft creates a real draft invoice on the
	// customer, for staff to open in the Stripe dashboard. It's never
	// finalized or sent, and it's deleted with the StripeInvoice.
	StripeInvoicePurposePreviewDraft StripeInvoicePurpose = "PreviewDraft"
)

const (
	// StripeInvoiceConditionReady is True once Stripe has worked out the
	// invoice and status holds its amounts. Reasons: Previewed, Drafted,
	// WaitingForCustomer, WaitingForProduct, StripeError.
	StripeInvoiceConditionReady = "Ready"
	// StripeInvoiceConditionAmountsMatch is False when Stripe's amount for
	// a line, before discounts, differs from the line's amount: its
	// quantity times unit price doesn't come to it.
	StripeInvoiceConditionAmountsMatch = "AmountsMatch"
)

// StripeInvoiceSpec defines the desired state of a StripeInvoice.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new StripeInvoice"
type StripeInvoiceSpec struct {
	// BillingAccountRef names the BillingAccount, in this namespace, whose
	// Stripe customer is invoiced. Its StripeCustomer must be Ready.
	//
	// +kubebuilder:validation:Required
	BillingAccountRef BillingAccountLocalRef `json:"billingAccountRef"`

	// Purpose is Preview or PreviewDraft.
	//
	// +kubebuilder:validation:Required
	Purpose StripeInvoicePurpose `json:"purpose"`

	// Currency is the ISO 4217 currency of the amounts. USD only.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^USD$`
	// +kubebuilder:default=USD
	Currency string `json:"currency,omitempty"`

	// Lines are the invoice's charges, in order.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=250
	// +listType=map
	// +listMapKey=key
	Lines []StripeInvoiceLine `json:"lines"`
}

// StripeInvoiceLine is one charge: an invoice item in Stripe.
//
// +kubebuilder:validation:XValidation:rule="!has(self.tax) || !has(self.tax.taxable) || self.tax.taxable || !has(self.tax.code)",message="tax.code can't be set on a line that isn't taxable"
type StripeInvoiceLine struct {
	// Key identifies the line within the invoice, and is kept on the
	// Stripe line's metadata.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Key string `json:"key"`

	// Description is what the invoice calls the charge.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=500
	Description string `json:"description"`

	// ServicePricingRef names the price charged. Its StripeProduct is the
	// line's Product, so it must be Ready.
	//
	// +kubebuilder:validation:Required
	ServicePricingRef ServicePricingRef `json:"servicePricingRef"`

	// Quantity is how many are charged, as a positive decimal string.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^([1-9][0-9]*(\.[0-9]+)?|0\.[0-9]*[1-9][0-9]*)$`
	Quantity string `json:"quantity"`

	// UnitAmount is the price of one, before discounts, as a decimal
	// string in the currency (e.g. "300.00").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)(\.[0-9]+)?$`
	UnitAmount string `json:"unitAmount"`

	// Amount is what the line should come to before discounts: what the
	// source charged. Stripe's amount is checked against it.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)(\.[0-9]+)?$`
	Amount string `json:"amount"`

	// Period is the service period the charge is for. Revenue Recognition
	// spreads the line over it.
	//
	// +kubebuilder:validation:Required
	Period StripeInvoiceLinePeriod `json:"period"`

	// Tax overrides the Product's tax code for this line.
	//
	// +kubebuilder:validation:Optional
	Tax *StripeInvoiceLineTax `json:"tax,omitempty"`

	// DiscountPercent is a percentage taken off the line, as a decimal
	// string (e.g. "10"). It's a Stripe coupon on the line.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^(100|[1-9]?[0-9])(\.[0-9]{1,2})?$`
	DiscountPercent string `json:"discountPercent,omitempty"`

	// Note is for staff and finance, kept in the line's metadata. It's
	// never printed on the invoice.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=500
	Note string `json:"note,omitempty"`

	// Metadata is copied onto the Stripe line, each key prefixed "datum_".
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxProperties=20
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ServicePricingRef names a billing ServicePricing.
type ServicePricingRef struct {
	// Namespace of the ServicePricing.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=milo-system
	Namespace string `json:"namespace,omitempty"`

	// Name of the ServicePricing.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// StripeInvoiceLinePeriod is a line's service period. Both ends are
// included.
//
// +kubebuilder:validation:XValidation:rule="self.end >= self.start",message="end can't be before start"
type StripeInvoiceLinePeriod struct {
	// +kubebuilder:validation:Required
	Start metav1.Time `json:"start"`
	// +kubebuilder:validation:Required
	End metav1.Time `json:"end"`
}

// StripeInvoiceLineTax is how one line is taxed.
//
// +kubebuilder:validation:MinProperties=1
type StripeInvoiceLineTax struct {
	// Code is the line's Stripe tax code, in place of its Product's.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	Code string `json:"code,omitempty"`

	// Taxable is false for a line that's never taxed.
	//
	// +kubebuilder:validation:Optional
	Taxable *bool `json:"taxable,omitempty"`
}

// StripeInvoiceStatus defines the observed state of a StripeInvoice.
// Amounts are decimal strings in the currency.
type StripeInvoiceStatus struct {
	// InvoiceID is the Stripe invoice: the draft for PreviewDraft. A
	// Preview creates nothing, so it has none.
	//
	// +kubebuilder:validation:Optional
	InvoiceID string `json:"invoiceId,omitempty"`

	// DashboardURL opens the draft in the Stripe dashboard.
	//
	// +kubebuilder:validation:Optional
	DashboardURL string `json:"dashboardUrl,omitempty"`

	// Lines are Stripe's amounts for each spec line, by key.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=key
	Lines []StripeInvoiceLineStatus `json:"lines,omitempty"`

	// Subtotal is the sum of the lines before discounts and tax.
	//
	// +kubebuilder:validation:Optional
	Subtotal string `json:"subtotal,omitempty"`

	// Discount is the sum of the discounts.
	//
	// +kubebuilder:validation:Optional
	Discount string `json:"discount,omitempty"`

	// Tax is the tax Stripe Tax charges, from the customer's address, tax
	// IDs and exemptions and each line's tax code.
	//
	// +kubebuilder:validation:Optional
	Tax string `json:"tax,omitempty"`

	// Total is the invoice's total: its lines after discounts, plus tax.
	//
	// +kubebuilder:validation:Optional
	Total string `json:"total,omitempty"`

	// StartingBalance is the customer's balance that Stripe carries onto
	// the invoice: positive for what they already owe, such as an earlier
	// invoice below Stripe's minimum charge, negative for credit.
	//
	// +kubebuilder:validation:Optional
	StartingBalance string `json:"startingBalance,omitempty"`

	// AmountDue is what the customer would be charged: Total with the
	// starting balance applied.
	//
	// +kubebuilder:validation:Optional
	AmountDue string `json:"amountDue,omitempty"`

	// ExpiresAt is when the StripeInvoice deletes itself: an hour after
	// it's created for a Preview, a day for a PreviewDraft, which takes
	// its draft with it.
	//
	// +kubebuilder:validation:Optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

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

// StripeInvoiceLineStatus is Stripe's amounts for one line.
type StripeInvoiceLineStatus struct {
	// Key is the spec line's key.
	//
	// +kubebuilder:validation:Required
	Key string `json:"key"`
	// Amount is Stripe's amount for the line, before discounts and tax.
	Amount string `json:"amount,omitempty"`
	// Discount is the line's discount.
	Discount string `json:"discount,omitempty"`
	// Tax is the line's tax.
	Tax string `json:"tax,omitempty"`
}

// StripeInvoice is the Schema for the stripeinvoices API.
//
// A StripeInvoice is an invoice for Stripe to work out, as lines with a
// Product, quantity, unit price, service period and tax. For now it only
// previews: Stripe's own preview with nothing created, or a draft that
// staff open in the Stripe dashboard and that's deleted after a day. Both
// use the same lines a real invoice will, so a preview can't drift from
// what's billed.
//
// Other controllers create StripeInvoices, in the billing account's
// namespace; staff don't. See docs/architecture/invoicing.md.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="BillingAccount",type=string,JSONPath=`.spec.billingAccountRef.name`
// +kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.spec.purpose`
// +kubebuilder:printcolumn:name="Total",type=string,JSONPath=`.status.total`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type StripeInvoice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StripeInvoiceSpec   `json:"spec,omitempty"`
	Status StripeInvoiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StripeInvoiceList contains a list of StripeInvoice.
type StripeInvoiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StripeInvoice `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StripeInvoice{}, &StripeInvoiceList{})
}
