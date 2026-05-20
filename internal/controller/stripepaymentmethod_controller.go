// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	stripeinternal "go.miloapis.com/stripe-provider/internal/stripe"
)

const (
	// stripePaymentMethodFinalizer guards Stripe-side cleanup. It is
	// added on first reconcile of an admitted resource and removed only
	// after the Stripe PaymentMethod has been detached from its
	// Customer (or the cleanup deadline has elapsed).
	stripePaymentMethodFinalizer = "stripe.billing.miloapis.com/cleanup"

	// defaultSetupIntentTTL is the lifetime of a freshly-created
	// SetupIntent. After this elapses without confirmation the
	// reconciler cancels the upstream SetupIntent and creates a new
	// one. Stripe's own server-side expiry is generous and inconsistent
	// across states; this gives us a deterministic floor so a forgotten
	// browser session doesn't leave a stale client secret.
	defaultSetupIntentTTL = 24 * time.Hour

	// stripeCleanupMaxAttempts caps the number of Detach attempts
	// before the controller gives up and clears the finalizer. The
	// design doc requires a bounded window so a Stripe outage cannot
	// permanently block deletion.
	stripeCleanupMaxAttempts = 12

	// stripeCleanupBackoff is the requeue interval between failed
	// Detach attempts. 5 minutes × 12 attempts = ~1 hour cleanup window.
	stripeCleanupBackoff = 5 * time.Minute

	// cleanupAttemptsAnnotation records the number of detach attempts
	// across reconciles. Kept on the CR itself rather than on status so
	// it survives the controller-runtime status cache and so the
	// counter doesn't lose state on rapid reconcile churn.
	cleanupAttemptsAnnotation = "stripe.billing.miloapis.com/cleanup-attempts"
)

// StripePaymentMethodReconciler drives the Stripe-side lifecycle: it
// ensures a Stripe Customer for the owning billing account, creates a
// SetupIntent, and writes clientSecret + status to the
// StripePaymentMethod. Webhook events from Stripe (handled in
// internal/webhook) move the resource into Active or Failed; once Active
// the controller projects the AwaitingConfirmation -> Active phase
// transition onto the parent PaymentMethod.
//
// On deletion the controller detaches the upstream PaymentMethod from
// the Stripe Customer before allowing the finalizer to be removed,
// preventing orphaned payment methods on the Stripe side. A bounded
// retry window means the resource is never permanently undeletable.
type StripePaymentMethodReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme

	// ProviderConfigName names the StripeProviderConfig the reconciler
	// uses for SDK credentials. Today only a single provider config is
	// supported per cluster; this is set from the operator flag.
	ProviderConfigName string

	// SetupIntentTTL overrides defaultSetupIntentTTL. Zero means use
	// the default.
	SetupIntentTTL time.Duration
}

// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods/finalizers,verbs=update
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *StripePaymentMethodReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var spm stripev1alpha1.StripePaymentMethod
	if err := r.Client.Get(ctx, req.NamespacedName, &spm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion path: detach the upstream PaymentMethod, then drop the finalizer.
	if !spm.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &spm)
	}

	// Ensure finalizer is present before doing any Stripe-side work.
	if !controllerutil.ContainsFinalizer(&spm, stripePaymentMethodFinalizer) {
		controllerutil.AddFinalizer(&spm, stripePaymentMethodFinalizer)
		if err := r.Client.Update(ctx, &spm); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Terminal states are sticky.
	switch spm.Status.Phase {
	case stripev1alpha1.StripePaymentMethodPhaseActive, stripev1alpha1.StripePaymentMethodPhaseFailed:
		return ctrl.Result{}, nil
	}

	// If a SetupIntent is in-flight, check whether it has expired
	// before treating it as still-valid.
	if spm.Status.SetupIntent != nil && spm.Status.SetupIntent.ClientSecret != "" {
		if !r.setupIntentExpired(spm.Status.SetupIntent) {
			// Still fresh — make sure the parent PaymentMethod reflects
			// AwaitingConfirmation and requeue for the next expiry check.
			if err := r.ensurePaymentMethodAwaiting(ctx, &spm); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: r.requeueUntilExpiry(spm.Status.SetupIntent)}, nil
		}
		logger.Info("SetupIntent expired without confirmation; recreating",
			"setupIntent", spm.Status.SetupIntent.ID)
		// Best-effort cancel of the stale upstream SetupIntent before
		// minting a replacement. Failures here are non-fatal; the new
		// SetupIntent will be the authoritative one.
		cfg, err := stripeinternal.ResolveConfig(ctx, r.Client, r.ProviderConfigName)
		if err == nil {
			_ = stripeinternal.NewClient(cfg).CancelSetupIntent(ctx, spm.Status.SetupIntent.ID)
		}
	}

	// Resolve dependencies + create / refresh the SetupIntent.
	return r.reconcileSetupIntent(ctx, &spm)
}

func (r *StripePaymentMethodReconciler) reconcileSetupIntent(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pm billingv1alpha1.PaymentMethod
	pmKey := types.NamespacedName{Namespace: spm.Namespace, Name: spm.Spec.PaymentMethodRef.Name}
	if err := r.Client.Get(ctx, pmKey, &pm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting parent PaymentMethod %s/%s: %w", pmKey.Namespace, pmKey.Name, err)
	}
	var ba billingv1alpha1.BillingAccount
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: pm.Spec.BillingAccountRef.Name}, &ba); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting BillingAccount %q: %w", pm.Spec.BillingAccountRef.Name, err)
	}

	cfg, err := stripeinternal.ResolveConfig(ctx, r.Client, r.ProviderConfigName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving StripeProviderConfig: %w", err)
	}
	stripe := stripeinternal.NewClient(cfg)

	customerID, err := stripe.EnsureCustomer(ctx, spm.Status.StripeCustomerID, ba.Name, customerDetailsFromBillingAccount(&ba))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Stripe customer: %w", err)
	}
	si, err := stripe.CreateSetupIntent(ctx, customerID, spm.Namespace, spm.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating Stripe SetupIntent: %w", err)
	}

	ttl := r.SetupIntentTTL
	if ttl <= 0 {
		ttl = defaultSetupIntentTTL
	}
	expiresAt := metav1.NewTime(time.Now().Add(ttl))

	base := spm.DeepCopy()
	spm.Status.Phase = stripev1alpha1.StripePaymentMethodPhaseAwaitingConfirmation
	spm.Status.StripeCustomerID = customerID
	spm.Status.SetupIntent = &stripev1alpha1.StripeSetupIntentStatus{
		ID:           si.ID,
		ClientSecret: si.ClientSecret,
		Status:       si.Status,
		ExpiresAt:    &expiresAt,
	}
	apimeta.SetStatusCondition(&spm.Status.Conditions, metav1.Condition{
		Type:               "SetupIntentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: spm.Generation,
		Reason:             "ClientSecretAvailable",
		Message:            fmt.Sprintf("Stripe SetupIntent %s created (expires %s).", si.ID, expiresAt.Format(time.RFC3339)),
	})
	spm.Status.ObservedGeneration = spm.Generation
	if err := r.Client.Status().Patch(ctx, spm, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching StripePaymentMethod status: %w", err)
	}
	logger.Info("created Stripe SetupIntent",
		"setupIntent", si.ID, "customer", customerID,
		"stripePaymentMethod", spm.Name, "expiresAt", expiresAt.Time)

	if err := r.ensurePaymentMethodAwaiting(ctx, spm); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: ttl}, nil
}

func (r *StripePaymentMethodReconciler) setupIntentExpired(si *stripev1alpha1.StripeSetupIntentStatus) bool {
	if si == nil || si.ExpiresAt == nil {
		// No expiry recorded — treat as fresh; the reconciler will
		// stamp one on the next successful creation.
		return false
	}
	return time.Now().After(si.ExpiresAt.Time)
}

func (r *StripePaymentMethodReconciler) requeueUntilExpiry(si *stripev1alpha1.StripeSetupIntentStatus) time.Duration {
	if si == nil || si.ExpiresAt == nil {
		return defaultSetupIntentTTL
	}
	d := time.Until(si.ExpiresAt.Time)
	if d <= 0 {
		return time.Second
	}
	return d
}

// reconcileDelete detaches the upstream Stripe PaymentMethod (and
// cancels any in-flight SetupIntent) before removing the finalizer. If
// the upstream API is unavailable, retries with a bounded budget and
// then surrenders the finalizer with a Degraded condition recording the
// incomplete cleanup.
func (r *StripePaymentMethodReconciler) reconcileDelete(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(spm, stripePaymentMethodFinalizer) {
		return ctrl.Result{}, nil
	}

	attempts := cleanupAttempts(spm)
	cfg, cfgErr := stripeinternal.ResolveConfig(ctx, r.Client, r.ProviderConfigName)
	var detachErr error
	if cfgErr != nil {
		detachErr = fmt.Errorf("resolving StripeProviderConfig: %w", cfgErr)
	} else {
		stripe := stripeinternal.NewClient(cfg)
		// Cancel any in-flight SetupIntent first; benign if already
		// terminal upstream.
		if spm.Status.SetupIntent != nil {
			if err := stripe.CancelSetupIntent(ctx, spm.Status.SetupIntent.ID); err != nil {
				detachErr = err
			}
		}
		if detachErr == nil {
			detachErr = stripe.DetachPaymentMethod(ctx, spm.Status.StripePaymentMethodID)
		}
	}

	if detachErr == nil {
		controllerutil.RemoveFinalizer(spm, stripePaymentMethodFinalizer)
		if err := r.Client.Update(ctx, spm); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer after detach: %w", err)
		}
		logger.Info("Stripe cleanup complete", "stripePaymentMethod", spm.Name)
		return ctrl.Result{}, nil
	}

	attempts++
	if attempts >= stripeCleanupMaxAttempts {
		// Surrender. The finalizer is removed so deletion can proceed;
		// the condition records that Stripe-side cleanup did not
		// complete so operators can detach the PaymentMethod by hand.
		base := spm.DeepCopy()
		apimeta.SetStatusCondition(&spm.Status.Conditions, metav1.Condition{
			Type:               "CleanupIncomplete",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: spm.Generation,
			Reason:             "DetachAttemptsExhausted",
			Message:            fmt.Sprintf("Gave up after %d detach attempts: %v. PaymentMethod %q may still be attached upstream.", attempts, detachErr, spm.Status.StripePaymentMethodID),
		})
		if err := r.Client.Status().Patch(ctx, spm, client.MergeFrom(base)); err != nil {
			logger.Error(err, "patching CleanupIncomplete condition (non-fatal)")
		}
		controllerutil.RemoveFinalizer(spm, stripePaymentMethodFinalizer)
		if err := r.Client.Update(ctx, spm); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer after exhausted attempts: %w", err)
		}
		logger.Error(detachErr, "Stripe cleanup exhausted attempts; finalizer removed",
			"stripePaymentMethod", spm.Name, "attempts", attempts)
		return ctrl.Result{}, nil
	}

	// Record the attempt count + condition, then requeue.
	if spm.Annotations == nil {
		spm.Annotations = make(map[string]string)
	}
	spm.Annotations[cleanupAttemptsAnnotation] = fmt.Sprintf("%d", attempts)
	if err := r.Client.Update(ctx, spm); err != nil {
		// On conflict, just requeue — the next reconcile will retry.
		return ctrl.Result{RequeueAfter: stripeCleanupBackoff}, nil
	}
	logger.Info("Stripe cleanup attempt failed; will retry",
		"stripePaymentMethod", spm.Name, "attempts", attempts, "err", detachErr.Error())
	return ctrl.Result{RequeueAfter: stripeCleanupBackoff}, nil
}

func cleanupAttempts(spm *stripev1alpha1.StripePaymentMethod) int {
	v := spm.Annotations[cleanupAttemptsAnnotation]
	if v == "" {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(v, "%d", &n)
	return n
}

// customerDetailsFromBillingAccount maps the billing CRD's
// vendor-neutral schema onto the SDK wrapper's CustomerDetails. The
// translation is one-way (BillingAccount → Stripe Customer) and lives
// entirely in this repo — billing.miloapis.com does not know what
// provider it is talking to.
//
// Invoice routing:
//   - When spec.billingDetails.invoiceEmail is set, it wins as the
//     Stripe Customer.email (Stripe's invoice + receipt recipient).
//   - Otherwise spec.contactInfo.email is used.
func customerDetailsFromBillingAccount(ba *billingv1alpha1.BillingAccount) stripeinternal.CustomerDetails {
	d := stripeinternal.CustomerDetails{}

	email := ""
	if ba.Spec.ContactInfo != nil {
		email = ba.Spec.ContactInfo.Email
	}
	if ba.Spec.BillingDetails != nil && ba.Spec.BillingDetails.InvoiceEmail != "" {
		email = ba.Spec.BillingDetails.InvoiceEmail
	}
	d.Email = email

	if ba.Spec.BillingDetails != nil && ba.Spec.BillingDetails.Address != nil {
		addr := ba.Spec.BillingDetails.Address
		d.Name = joinName(addr.FirstName, addr.LastName)
		d.Address = &stripeinternal.CustomerAddress{
			Country:    addr.Country,
			Line1:      addr.Line1,
			Line2:      addr.Line2,
			City:       addr.City,
			State:      addr.Region,
			PostalCode: addr.PostalCode,
		}
	}
	// Fall back to contact name when no address-derived name is available.
	if d.Name == "" && ba.Spec.ContactInfo != nil {
		d.Name = ba.Spec.ContactInfo.Name
	}

	if ba.Spec.BillingDetails != nil {
		for _, t := range ba.Spec.BillingDetails.TaxIDs {
			d.TaxIDs = append(d.TaxIDs, stripeinternal.TaxIDDetails{Type: t.Type, Value: t.Value})
		}
	}
	return d
}

func joinName(first, last string) string {
	switch {
	case first != "" && last != "":
		return first + " " + last
	case first != "":
		return first
	case last != "":
		return last
	}
	return ""
}

// ensurePaymentMethodAwaiting moves the parent PaymentMethod phase to
// AwaitingConfirmation once the Stripe side has a client secret ready.
// Idempotent: if the parent already reports Awaiting/Active/Failed it
// leaves it alone.
func (r *StripePaymentMethodReconciler) ensurePaymentMethodAwaiting(ctx context.Context, spm *stripev1alpha1.StripePaymentMethod) error {
	var pm billingv1alpha1.PaymentMethod
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: spm.Namespace, Name: spm.Spec.PaymentMethodRef.Name}, &pm); err != nil {
		return fmt.Errorf("getting parent PaymentMethod: %w", err)
	}
	if pm.Status.Phase == billingv1alpha1.PaymentMethodPhaseAwaitingConfirmation ||
		pm.Status.Phase == billingv1alpha1.PaymentMethodPhaseActive ||
		pm.Status.Phase == billingv1alpha1.PaymentMethodPhaseFailed {
		return nil
	}
	base := pm.DeepCopy()
	pm.Status.Phase = billingv1alpha1.PaymentMethodPhaseAwaitingConfirmation
	apimeta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.PaymentMethodConditionInstrumentReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: pm.Generation,
		Reason:             "AwaitingConfirmation",
		Message:            "Stripe SetupIntent created; awaiting consumer confirmation.",
	})
	return r.Client.Status().Patch(ctx, &pm, client.MergeFrom(base))
}

// SetupWithManager wires the reconciler.
func (r *StripePaymentMethodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	return ctrl.NewControllerManagedBy(mgr).
		Named("stripepaymentmethod").
		For(&stripev1alpha1.StripePaymentMethod{}).
		Complete(r)
}
