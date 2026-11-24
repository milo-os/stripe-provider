// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

const (
	// reconcileTimeout bounds every reconcile, so a request to Milo or
	// Stripe that never returns is cancelled and retried rather than
	// holding its controller's worker for good.
	reconcileTimeout = 5 * time.Minute

	// miloCheckTimeout bounds each liveness check's request to Milo.
	miloCheckTimeout = 10 * time.Second
	// miloUnreachableAfter is how long Milo must go unanswered before
	// liveness fails and the kubelet restarts the pod. Long enough that
	// a blip or a Milo rollout doesn't restart it.
	miloUnreachableAfter = 2 * time.Minute
)

// miloCheck is a liveness check on the connection the controllers use.
// Leader election has a connection of its own, so a replica can keep its
// lease while every request on the controllers' connection hangs: it stays
// leader and does nothing. This check reads the provider config through
// the controllers' client, uncached, and fails once nothing has come back
// for miloUnreachableAfter.
type miloCheck struct {
	reader client.Reader
	name   string
	now    func() time.Time

	mu       sync.Mutex
	lastOK   time.Time
	lastFail error
}

func newMiloCheck(reader client.Reader, providerConfigName string) *miloCheck {
	c := &miloCheck{reader: reader, name: providerConfigName, now: time.Now}
	// Count from start, so a replica that never reaches Milo fails too.
	c.lastOK = c.now()
	return c
}

// Check is a healthz.Checker.
func (c *miloCheck) Check(req *http.Request) error {
	ctx, cancel := context.WithTimeout(req.Context(), miloCheckTimeout)
	defer cancel()
	err := c.reader.Get(ctx, types.NamespacedName{Name: c.name}, &stripev1alpha1.StripeProviderConfig{})

	c.mu.Lock()
	defer c.mu.Unlock()
	// Any answer from Milo, a refusal included, shows the connection works.
	if err == nil || isAPIAnswer(err) {
		c.lastOK, c.lastFail = c.now(), nil
		return nil
	}
	c.lastFail = err
	if since := c.now().Sub(c.lastOK); since > miloUnreachableAfter {
		return fmt.Errorf("no answer from Milo for %s: %w", since.Round(time.Second), err)
	}
	return nil
}

// isAPIAnswer reports whether err is a response from the API server rather
// than a failure to reach it.
func isAPIAnswer(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	return !apierrors.IsTimeout(err) && !apierrors.IsServerTimeout(err)
}
