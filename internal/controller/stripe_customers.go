// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// StripeCustomers creates and syncs the single Stripe Customer each
// BillingAccount has, and publishes its ID on the account's
// StripeCustomer resource.
//
// Both the BillingAccountReconciler (for accounts billed by invoice,
// which need a customer without ever adding a card) and the
// StripePaymentMethodReconciler (for the customer a SetupIntent is
// created against) go through Ensure, so one instance must be shared
// between them. Ensure holds a per-account lock and reads the
// StripeCustomer without the cache, so two reconcilers seeing the same
// account at once can't both find no customer and create one each.
// Stripe's customer search is eventually consistent, so it can't be
// relied on to catch that race by itself.
type StripeCustomers struct {
	client client.Client
	// reader reads StripeCustomers straight from the API server: a
	// cached read could miss a customer ID another reconciler published
	// moments ago.
	reader client.Reader

	mu    sync.Mutex
	locks map[types.NamespacedName]*sync.Mutex
}

// NewStripeCustomers returns a StripeCustomers that writes with c and
// reads StripeCustomers with reader, which should be uncached
// (manager.GetAPIReader).
func NewStripeCustomers(c client.Client, reader client.Reader) *StripeCustomers {
	return &StripeCustomers{client: c, reader: reader, locks: map[types.NamespacedName]*sync.Mutex{}}
}

func (s *StripeCustomers) lock(key types.NamespacedName) func() {
	s.mu.Lock()
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Ensure creates the account's StripeCustomer if it has none, then
// creates or updates the Stripe Customer it names, and records the
// customer ID and sync conditions on its status. Returns the customer
// ID.
//
// The customer ID comes from, in order: the StripeCustomer's status; a
// StripePaymentMethod created for the account before StripeCustomer
// existed; Stripe's customer search (inside EnsureCustomer). Only when
// all three come up empty is a new Stripe Customer created.
//
// A *stripeinternal.TaxIDError is returned alongside a valid customer
// ID: the customer synced, but Stripe rejected a tax ID. Callers should
// carry on with the ID.
func (s *StripeCustomers) Ensure(
	ctx context.Context,
	stripe stripeCustomerEnsurer,
	ba *billingv1alpha1.BillingAccount,
) (string, error) {
	defer s.lock(types.NamespacedName{Namespace: ba.Namespace, Name: ba.Name})()

	sc, err := s.getOrCreate(ctx, ba)
	if err != nil {
		return "", err
	}

	existingID := sc.Status.StripeCustomerID
	if existingID == "" {
		existingID, err = findExistingCustomerID(ctx, s.client, ba.Namespace, ba.Name)
		if err != nil {
			return "", fmt.Errorf("looking up existing Stripe customer for BillingAccount %q: %w", ba.Name, err)
		}
	}

	details, err := buildCustomerDetails(ctx, s.client, ba)
	if err != nil {
		return "", err
	}
	account := stripeinternal.BillingAccountKey{Namespace: ba.Namespace, Name: ba.Name, UID: string(ba.UID)}
	customerID, ensureErr := stripe.EnsureCustomer(ctx, existingID, account, details)

	var taxErr *stripeinternal.TaxIDError
	if ensureErr != nil && !errors.As(ensureErr, &taxErr) {
		// Keep the ID already published: the customer still exists,
		// this sync just didn't reach it.
		if statusErr := s.patchStatus(ctx, sc, existingID, metav1.Condition{
			Type:    stripev1alpha1.StripeCustomerConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "StripeError",
			Message: fmt.Sprintf("Syncing the Stripe customer failed: %v", ensureErr),
		}, nil); statusErr != nil {
			return "", errors.Join(ensureErr, statusErr)
		}
		return "", ensureErr
	}

	taxIDs := metav1.Condition{
		Type:    stripev1alpha1.StripeCustomerConditionTaxIDsApplied,
		Status:  metav1.ConditionTrue,
		Reason:  "Synced",
		Message: "Tax IDs reconciled against Stripe customer.",
	}
	if taxErr != nil {
		taxIDs = metav1.Condition{
			Type:    stripev1alpha1.StripeCustomerConditionTaxIDsApplied,
			Status:  metav1.ConditionFalse,
			Reason:  "StripeRejected",
			Message: fmt.Sprintf("Stripe rejected one or more tax IDs on the customer; correct the values on the BillingAccount: %v", taxErr.Underlying),
		}
	}
	if err := s.patchStatus(ctx, sc, customerID, metav1.Condition{
		Type:    stripev1alpha1.StripeCustomerConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "CustomerSynced",
		Message: fmt.Sprintf("Stripe customer %s is in sync with the BillingAccount.", customerID),
	}, &taxIDs); err != nil {
		return "", err
	}
	return customerID, ensureErr
}

// getOrCreate returns the account's StripeCustomer, creating it (named
// after the account and controlled by it) when it doesn't exist.
func (s *StripeCustomers) getOrCreate(ctx context.Context, ba *billingv1alpha1.BillingAccount) (*stripev1alpha1.StripeCustomer, error) {
	key := client.ObjectKey{Namespace: ba.Namespace, Name: ba.Name}
	var sc stripev1alpha1.StripeCustomer
	err := s.reader.Get(ctx, key, &sc)
	if apierrors.IsNotFound(err) {
		sc = stripev1alpha1.StripeCustomer{
			ObjectMeta: metav1.ObjectMeta{Namespace: ba.Namespace, Name: ba.Name},
			Spec: stripev1alpha1.StripeCustomerSpec{
				BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: ba.Name},
			},
		}
		if err := controllerutil.SetControllerReference(ba, &sc, s.client.Scheme()); err != nil {
			return nil, fmt.Errorf("setting owner on StripeCustomer %q: %w", ba.Name, err)
		}
		if err := s.client.Create(ctx, &sc); err != nil {
			return nil, fmt.Errorf("creating StripeCustomer %q: %w", ba.Name, err)
		}
		return &sc, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting StripeCustomer %q: %w", ba.Name, err)
	}
	// A StripeCustomer left behind by an earlier BillingAccount with the
	// same name names that account's customer. Wait for garbage
	// collection to remove it rather than bill the new account to it.
	if owner := metav1.GetControllerOf(&sc); owner == nil || owner.UID != ba.UID {
		return nil, fmt.Errorf("StripeCustomer %q is not owned by BillingAccount %s; waiting for it to be garbage collected", ba.Name, ba.UID)
	}
	return &sc, nil
}

// patchStatus records customerID and the given conditions on sc,
// skipping the write when nothing changed.
func (s *StripeCustomers) patchStatus(
	ctx context.Context,
	sc *stripev1alpha1.StripeCustomer,
	customerID string,
	ready metav1.Condition,
	taxIDs *metav1.Condition,
) error {
	base := sc.DeepCopy()
	sc.Status.StripeCustomerID = customerID
	ready.ObservedGeneration = sc.Generation
	apimeta.SetStatusCondition(&sc.Status.Conditions, ready)
	if taxIDs != nil {
		taxIDs.ObservedGeneration = sc.Generation
		apimeta.SetStatusCondition(&sc.Status.Conditions, *taxIDs)
	}
	sc.Status.ObservedGeneration = sc.Generation
	if apiequality.Semantic.DeepEqual(base.Status, sc.Status) {
		return nil
	}
	if err := s.client.Status().Patch(ctx, sc, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patching StripeCustomer %q status: %w", sc.Name, err)
	}
	return nil
}
