// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stubReader answers every Get with err, and counts the calls.
type stubReader struct {
	err   error
	calls atomic.Int32
}

func (r *stubReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	r.calls.Add(1)
	return r.err
}

func (r *stubReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

// TestMiloCheck checks liveness holds while Milo answers, refusals
// included, and through a short outage, and fails once Milo has gone
// unanswered for miloUnreachableAfter.
func TestMiloCheck(t *testing.T) {
	reader := &stubReader{}
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	c := newMiloCheck(reader, "default")
	c.now = func() time.Time { return now }
	c.lastOK = now
	ctx := context.Background()
	check := func() error { return c.Check(httptest.NewRequest("GET", "/healthz", nil)) }

	c.probe(ctx)
	if err := check(); err != nil {
		t.Fatalf("Milo answering: %v", err)
	}

	gr := schema.GroupResource{Group: "stripe.billing.miloapis.com", Resource: "stripeproviderconfigs"}
	for name, refusal := range map[string]error{
		"not found": apierrors.NewNotFound(gr, "default"),
		"forbidden": apierrors.NewForbidden(gr, "default", errors.New("no")),
	} {
		reader.err = refusal
		now = now.Add(time.Hour)
		c.probe(ctx)
		if err := check(); err != nil {
			t.Errorf("%s from Milo: %v, want healthy", name, err)
		}
	}

	reader.err = context.DeadlineExceeded
	now = now.Add(miloUnreachableAfter / 2)
	c.probe(ctx)
	if err := check(); err != nil {
		t.Errorf("unanswered for a short while: %v, want healthy", err)
	}
	now = now.Add(miloUnreachableAfter)
	c.probe(ctx)
	if err := check(); err == nil {
		t.Error("unanswered past miloUnreachableAfter: healthy, want an error")
	}

	reader.err = apierrors.NewTimeoutError("slow", 1)
	c.probe(ctx)
	if err := check(); err == nil {
		t.Error("a timeout from Milo counted as an answer")
	}

	reader.err = nil
	c.probe(ctx)
	if err := check(); err != nil {
		t.Errorf("Milo answering again: %v", err)
	}
}

// TestMiloCheckDoesNotCallMilo checks a probe answers from the last result
// without a request of its own, so a slow Milo can't outlast the kubelet's
// probe timeout.
func TestMiloCheckDoesNotCallMilo(t *testing.T) {
	reader := &stubReader{}
	c := newMiloCheck(reader, "default")
	for range 3 {
		if err := c.Check(httptest.NewRequest("GET", "/healthz", nil)); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	if n := reader.calls.Load(); n != 0 {
		t.Errorf("Check called Milo %d times, want 0", n)
	}
}

// TestMiloCheckStart checks the background loop asks Milo at once and
// stops with its context.
func TestMiloCheckStart(t *testing.T) {
	reader := &stubReader{}
	c := newMiloCheck(reader, "default")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for reader.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if reader.calls.Load() == 0 {
		t.Fatal("Start didn't ask Milo")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start didn't stop with its context")
	}
	if c.NeedLeaderElection() {
		t.Error("NeedLeaderElection = true, want every replica to check")
	}
}
