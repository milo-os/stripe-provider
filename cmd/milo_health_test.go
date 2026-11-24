// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stubReader answers every Get with err.
type stubReader struct{ err error }

func (r *stubReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
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
	check := func() error { return c.Check(httptest.NewRequest("GET", "/healthz", nil)) }

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
		if err := check(); err != nil {
			t.Errorf("%s from Milo: %v, want healthy", name, err)
		}
	}

	reader.err = context.DeadlineExceeded
	now = now.Add(miloUnreachableAfter / 2)
	if err := check(); err != nil {
		t.Errorf("unanswered for a short while: %v, want healthy", err)
	}
	now = now.Add(miloUnreachableAfter)
	if err := check(); err == nil {
		t.Error("unanswered past miloUnreachableAfter: healthy, want an error")
	}

	reader.err = apierrors.NewTimeoutError("slow", 1)
	if err := check(); err == nil {
		t.Error("a timeout from Milo counted as an answer")
	}

	reader.err = nil
	if err := check(); err != nil {
		t.Errorf("Milo answering again: %v", err)
	}
}
