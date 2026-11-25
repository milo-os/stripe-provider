// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

// customerPrefixer is the part of Stripe the invoice number check needs.
type customerPrefixer interface {
	CustomerInvoicePrefix(ctx context.Context, customerID string) (string, error)
	SetCustomerInvoicePrefix(ctx context.Context, customerID, prefix string) error
}

// maxPrefixAttempts is how many numbers one check hands out before giving
// up, when Stripe refuses each as taken. A refusal is rare; several in a
// row means something else is wrong, and the next reconcile tries again.
const maxPrefixAttempts = 5

// errPrefixTakenAfterUse is returned when Stripe refuses a customer number
// that the customer's invoices already carry. Handing out another would
// change the customer's number, so staff sort it out in Stripe.
var errPrefixTakenAfterUse = errors.New("stripe refused the customer's own invoice prefix")

// EnsureInvoicePrefix makes sure the account's Stripe customer numbers its
// invoices with the account's customer number, just before an invoice is
// finalized. It hands out a number if the account has none, then reads the
// prefix from Stripe and sets it if it's wrong: a crash between recording
// a number and setting it, or a change made in the Stripe dashboard, is
// put right before the invoice takes its number.
//
// It does nothing when the StripeProviderConfig doesn't exist or has no
// spec.invoiceNumbers. onRefused is called with each number Stripe refuses
// and the provider gives up on. See docs/enhancements/invoice-numbers.md
// in the billing repository.
func (s *StripeCustomers) EnsureInvoicePrefix(
	ctx context.Context,
	stripe customerPrefixer,
	configName string,
	key types.NamespacedName,
	onRefused func(number int64),
) error {
	var cfg stripev1alpha1.StripeProviderConfig
	if err := s.reader.Get(ctx, types.NamespacedName{Name: configName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting StripeProviderConfig %q: %w", configName, err)
	}
	if cfg.Spec.InvoiceNumbers == nil {
		return nil
	}

	// The account's lock, as for Ensure: both write StripeCustomer status.
	defer s.lock(key)()
	var sc stripev1alpha1.StripeCustomer
	if err := s.reader.Get(ctx, key, &sc); err != nil {
		return fmt.Errorf("getting StripeCustomer %s: %w", key, err)
	}
	customerID := sc.Status.StripeCustomerID
	if customerID == "" {
		return fmt.Errorf("StripeCustomer %s has no Stripe customer yet", key)
	}

	for range maxPrefixAttempts {
		if sc.Status.CustomerNumber == 0 {
			n, err := s.nextCustomerNumber(ctx, configName)
			if err != nil {
				return err
			}
			if err := s.patchCustomerStatus(ctx, &sc, func(st *stripev1alpha1.StripeCustomerStatus) { st.CustomerNumber = n }); err != nil {
				return err
			}
		}
		number := sc.Status.CustomerNumber
		want := strconv.FormatInt(number, 10)

		have, err := stripe.CustomerInvoicePrefix(ctx, customerID)
		if err != nil {
			return err
		}
		if have != want {
			err := stripe.SetCustomerInvoicePrefix(ctx, customerID, want)
			if errors.Is(err, stripeinternal.ErrInvoicePrefixTaken) {
				if apimeta.IsStatusConditionTrue(sc.Status.Conditions, stripev1alpha1.StripeCustomerConditionInvoicePrefixSet) {
					return fmt.Errorf("%w: %s is taken in Stripe, but customer %s's invoices already carry it; fix the customer's invoice prefix in Stripe",
						errPrefixTakenAfterUse, want, customerID)
				}
				// No invoice carries the number yet: replace it.
				if onRefused != nil {
					onRefused(number)
				}
				sc.Status.CustomerNumber = 0
				continue
			}
			if err != nil {
				return err
			}
		}
		if !apimeta.IsStatusConditionTrue(sc.Status.Conditions, stripev1alpha1.StripeCustomerConditionInvoicePrefixSet) {
			return s.patchCustomerStatus(ctx, &sc, func(st *stripev1alpha1.StripeCustomerStatus) {
				apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
					Type: stripev1alpha1.StripeCustomerConditionInvoicePrefixSet, Status: metav1.ConditionTrue,
					Reason: "PrefixSet", ObservedGeneration: sc.Generation,
					Message: fmt.Sprintf("Stripe numbers this customer's invoices %s-NNNN", want),
				})
			})
		}
		return nil
	}
	return fmt.Errorf("stripe refused %d customer numbers in a row for customer %s", maxPrefixAttempts, customerID)
}

// nextCustomerNumber hands out the next customer number. The counter on the
// config's status moves first, with an update that fails on a conflicting
// write, so a crash later can skip a number but never hand one out twice.
func (s *StripeCustomers) nextCustomerNumber(ctx context.Context, configName string) (int64, error) {
	s.numbers.Lock()
	defer s.numbers.Unlock()

	if !s.counterChecked {
		if err := s.raiseCounter(ctx, configName); err != nil {
			return 0, err
		}
		s.counterChecked = true
	}

	var n int64
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cfg, err := s.getConfig(ctx, configName)
		if err != nil {
			return err
		}
		n = max(cfg.Status.LastCustomerNumber, cfg.Spec.InvoiceNumbers.FirstCustomerNumber-1) + 1
		cfg.Status.LastCustomerNumber = n
		return s.client.Status().Update(ctx, cfg)
	})
	if err != nil {
		return 0, fmt.Errorf("handing out a customer number: %w", err)
	}
	return n, nil
}

// raiseCounter raises the counter to the highest number on any
// StripeCustomer. Status isn't backed up, so a recreated config starts
// with an empty counter; the numbers its customers hold must not be handed
// out again.
func (s *StripeCustomers) raiseCounter(ctx context.Context, configName string) error {
	var list stripev1alpha1.StripeCustomerList
	if err := s.reader.List(ctx, &list); err != nil {
		return fmt.Errorf("listing StripeCustomers: %w", err)
	}
	var highest int64
	for _, sc := range list.Items {
		highest = max(highest, sc.Status.CustomerNumber)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cfg, err := s.getConfig(ctx, configName)
		if err != nil {
			return err
		}
		if cfg.Status.LastCustomerNumber >= highest {
			return nil
		}
		cfg.Status.LastCustomerNumber = highest
		return s.client.Status().Update(ctx, cfg)
	})
}

func (s *StripeCustomers) getConfig(ctx context.Context, name string) (*stripev1alpha1.StripeProviderConfig, error) {
	var cfg stripev1alpha1.StripeProviderConfig
	if err := s.reader.Get(ctx, types.NamespacedName{Name: name}, &cfg); err != nil {
		return nil, fmt.Errorf("getting StripeProviderConfig %q: %w", name, err)
	}
	if cfg.Spec.InvoiceNumbers == nil {
		return nil, fmt.Errorf("StripeProviderConfig %q no longer has spec.invoiceNumbers", name)
	}
	return &cfg, nil
}

// patchCustomerStatus writes a change to sc's status, failing if sc changed
// since it was read.
func (s *StripeCustomers) patchCustomerStatus(ctx context.Context, sc *stripev1alpha1.StripeCustomer, mutate func(*stripev1alpha1.StripeCustomerStatus)) error {
	base := sc.DeepCopy()
	mutate(&sc.Status)
	if err := s.client.Status().Patch(ctx, sc, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("updating StripeCustomer %s status: %w", sc.Name, err)
	}
	return nil
}
