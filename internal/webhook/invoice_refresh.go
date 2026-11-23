// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"fmt"
	"strings"

	stripego "github.com/stripe/stripe-go/v87"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	"go.miloapis.com/stripe-provider/internal/controller"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeinvoices,verbs=get;patch

// refreshStripeInvoice has the StripeInvoice that wrote an invoice read it
// from Stripe again, by stamping the event's id on it: Stripe's events say
// that something changed, and the controller reads what. Invoices we didn't
// write (staff's, preview drafts) are left alone, as is an event for a
// StripeInvoice that's gone or isn't the one the invoice names.
func (wh *Webhook) refreshStripeInvoice(ctx context.Context, event *stripego.Event) error {
	inv, err := decodeInvoice(event)
	if err != nil {
		return err
	}
	if inv.Metadata[stripeinternal.MetadataPurpose] != string(stripev1alpha1.StripeInvoicePurposeInvoice) {
		return nil
	}
	ns, name, ok := strings.Cut(inv.Metadata[stripeinternal.MetadataStripeInvoice], "/")
	if !ok || ns == "" || name == "" {
		return nil
	}
	var si stripev1alpha1.StripeInvoice
	if err := wh.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &si); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting StripeInvoice %s/%s: %w", ns, name, err)
	}
	if uid := inv.Metadata[stripeinternal.MetadataStripeInvoiceUID]; uid != "" && uid != string(si.UID) {
		return nil
	}
	if si.Annotations[controller.StripeInvoiceRefreshAnnotation] == event.ID {
		return nil
	}
	patch := client.MergeFrom(si.DeepCopy())
	if si.Annotations == nil {
		si.Annotations = map[string]string{}
	}
	si.Annotations[controller.StripeInvoiceRefreshAnnotation] = event.ID
	if err := wh.Client.Patch(ctx, &si, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("patching StripeInvoice %s/%s: %w", ns, name, err)
	}
	return nil
}
