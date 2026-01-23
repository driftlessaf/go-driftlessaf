/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package ocireconciler

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-containerregistry/pkg/name"
	crregistry "github.com/google/go-containerregistry/pkg/registry"
)

const testDigest = "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"

// TestReconcileParseFailure verifies that invalid digest keys return a non-retriable error.
func TestReconcileParseFailure(t *testing.T) {
	r := New(WithReconciler(func(context.Context, name.Digest) error {
		t.Fatal("reconciler should not have been invoked")
		return nil
	}))

	err := r.Reconcile(context.Background(), "not-a-digest")
	if err == nil {
		t.Fatal("error: got = nil, wanted = non-nil")
	}
	if details := workqueue.GetNonRetriableDetails(err); details == nil {
		t.Errorf("non-retriable details: got = nil, wanted = non-nil")
	}
}

// TestReconcileDelegates verifies that valid digests are parsed and passed to the reconciler func.
func TestReconcileDelegates(t *testing.T) {
	called := false
	r := New(WithReconciler(func(_ context.Context, digest name.Digest) error {
		called = true
		if got, want := digest.DigestStr(), testDigest; got != want {
			t.Errorf("digest: got = %s, wanted = %s", got, want)
		}
		return nil
	}))

	if err := r.Reconcile(context.Background(), "cgr.dev/example@"+testDigest); err != nil {
		t.Fatalf("Reconcile error: got = %v, wanted = nil", err)
	}
	if !called {
		t.Error("reconciler invoked: got = false, wanted = true")
	}
}

// TestProcessPropagatesErrors verifies that reconciler errors are propagated through Process.
func TestProcessPropagatesErrors(t *testing.T) {
	want := errors.New("boom")
	r := New(WithReconciler(func(context.Context, name.Digest) error {
		return want
	}))

	_, err := r.Process(context.Background(), &workqueue.ProcessRequest{Key: "cgr.dev/example@" + testDigest})
	if !errors.Is(err, want) {
		t.Errorf("Process error: got = %v, wanted = %v", err, want)
	}
}

// TestReconcileNameOptionsApplied verifies that name.Options are passed through to digest parsing.
func TestReconcileNameOptionsApplied(t *testing.T) {
	var got string
	r := New(
		WithNameOptions(name.WithDefaultRegistry("registry.dev")),
		WithReconciler(func(_ context.Context, digest name.Digest) error {
			got = digest.RegistryStr()
			return nil
		}),
	)

	if err := r.Reconcile(context.Background(), "example@"+testDigest); err != nil {
		t.Fatalf("Reconcile error: got = %v, wanted = nil", err)
	}
	if want := "registry.dev"; got != want {
		t.Errorf("registry: got = %q, wanted = %q", got, want)
	}
}

// TestReconcileWithRegistryFixture verifies reconciliation with a real registry server.
func TestReconcileWithRegistryFixture(t *testing.T) {
	srv := httptest.NewServer(crregistry.New())
	t.Cleanup(srv.Close)

	called := false
	r := New(WithReconciler(func(context.Context, name.Digest) error {
		called = true
		return nil
	}))

	ref := strings.TrimPrefix(srv.URL, "http://") + "/repo@" + testDigest
	if err := r.Reconcile(context.Background(), ref); err != nil {
		t.Fatalf("Reconcile error: got = %v, wanted = nil", err)
	}
	if !called {
		t.Error("reconciler invoked: got = false, wanted = true")
	}
}

// TestProcessRequeue verifies that RequeueAfter errors result in appropriate requeue delays.
func TestProcessRequeue(t *testing.T) {
	want := time.Minute
	r := New(WithReconciler(func(context.Context, name.Digest) error {
		return workqueue.RequeueAfter(want)
	}))

	resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{Key: "cgr.dev/example@" + testDigest})
	if err != nil {
		t.Fatalf("Process error: got = %v, wanted = nil", err)
	}
	if got := resp.RequeueAfterSeconds; got != int64(want.Seconds()) {
		t.Errorf("RequeueAfterSeconds: got = %d, wanted = %d", got, int64(want.Seconds()))
	}
}

// TestProcessNonRetriable verifies that non-retriable errors return success with no requeue.
func TestProcessNonRetriable(t *testing.T) {
	r := New(WithReconciler(func(context.Context, name.Digest) error {
		return workqueue.NonRetriableError(errors.New("boom"), "fail")
	}))

	resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{Key: "cgr.dev/example@" + testDigest})
	if err != nil {
		t.Fatalf("Process error: got = %v, wanted = nil", err)
	}
	if got := resp.RequeueAfterSeconds; got != 0 {
		t.Errorf("RequeueAfterSeconds: got = %d, wanted = 0", got)
	}
}

// TestProcessNoReconcilerConfigured verifies that Process fails when no reconciler is set.
func TestProcessNoReconcilerConfigured(t *testing.T) {
	r := New()
	_, err := r.Process(context.Background(), &workqueue.ProcessRequest{Key: "cgr.dev/example@" + testDigest})
	if err == nil {
		t.Error("error: got = nil, wanted = non-nil")
	}
}

// TestProcessHappyPathWithRegistry verifies end-to-end processing with a registry fixture.
func TestProcessHappyPathWithRegistry(t *testing.T) {
	srv := httptest.NewServer(crregistry.New())
	t.Cleanup(srv.Close)

	var received name.Digest
	r := New(WithReconciler(func(_ context.Context, digest name.Digest) error {
		received = digest
		return nil
	}))

	key := strings.TrimPrefix(srv.URL, "http://") + "/repo@" + testDigest
	resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{Key: key})
	if err != nil {
		t.Fatalf("Process error: got = %v, wanted = nil", err)
	}
	if got := resp.RequeueAfterSeconds; got != 0 {
		t.Errorf("RequeueAfterSeconds: got = %d, wanted = 0", got)
	}

	want, err := name.NewDigest(key)
	if err != nil {
		t.Fatalf("parsing key: %v", err)
	}
	if got := received.Repository.String(); got != want.Repository.String() {
		t.Errorf("repository: got = %q, wanted = %q", got, want.Repository.String())
	}
	if got := received.DigestStr(); got != want.DigestStr() {
		t.Errorf("digest: got = %q, wanted = %q", got, want.DigestStr())
	}
}
