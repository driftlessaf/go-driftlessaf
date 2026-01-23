/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package apkreconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/apkreconciler/apkurl"
	"chainguard.dev/driftlessaf/workqueue"
)

const (
	validRoot  = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	validChild = "1234567890abcdef"
	validKey   = "apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.apk"
)

func TestNew(t *testing.T) {
	t.Run("without options", func(t *testing.T) {
		r := New()
		if r == nil {
			t.Fatal("New() returned nil")
		}
		if r.reconcileFunc != nil {
			t.Error("New() without options should have nil reconcileFunc")
		}
	})

	t.Run("with reconciler option", func(t *testing.T) {
		called := false
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			called = true
			return nil
		}))

		if r.reconcileFunc == nil {
			t.Fatal("WithReconciler should set reconcileFunc")
		}

		// Verify the function was set correctly by calling it
		err := r.Reconcile(context.Background(), validKey)
		if err != nil {
			t.Errorf("Reconcile() error = %v", err)
		}
		if !called {
			t.Error("reconcileFunc was not called")
		}
	})
}

func TestReconcile(t *testing.T) {
	t.Run("no reconciler configured", func(t *testing.T) {
		r := New()
		err := r.Reconcile(context.Background(), validKey)
		if err == nil {
			t.Fatal("Reconcile() should error when no reconciler configured")
		}
		if err.Error() != "no reconciler configured" {
			t.Errorf("Reconcile() error = %q, want %q", err.Error(), "no reconciler configured")
		}
	})

	t.Run("valid key", func(t *testing.T) {
		var receivedKey *apkurl.Key
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			receivedKey = key
			return nil
		}))

		err := r.Reconcile(context.Background(), validKey)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		if receivedKey == nil {
			t.Fatal("reconcileFunc did not receive key")
		}
		if receivedKey.Host != "apk.cgr.dev" {
			t.Errorf("key.Host = %q, want %q", receivedKey.Host, "apk.cgr.dev")
		}
		if receivedKey.RepoPath != validRoot {
			t.Errorf("key.RepoPath = %q, want %q", receivedKey.RepoPath, validRoot)
		}
		if receivedKey.Package.Name != "glibc" {
			t.Errorf("key.Package.Name = %q, want %q", receivedKey.Package.Name, "glibc")
		}
		if receivedKey.Package.Version != "2.42-r0" {
			t.Errorf("key.Package.Version = %q, want %q", receivedKey.Package.Version, "2.42-r0")
		}
		if receivedKey.Package.Arch != "x86_64" {
			t.Errorf("key.Package.Arch = %q, want %q", receivedKey.Package.Arch, "x86_64")
		}
	})

	t.Run("valid key with multi-part repo path", func(t *testing.T) {
		key := "apk.cgr.dev/" + validRoot + "/" + validChild + "/aarch64/openssl-3.1.0-r5.apk"
		var receivedKey *apkurl.Key
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			receivedKey = key
			return nil
		}))

		err := r.Reconcile(context.Background(), key)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		if receivedKey.RepoPath != validRoot+"/"+validChild {
			t.Errorf("key.RepoPath = %q, want %q", receivedKey.RepoPath, validRoot+"/"+validChild)
		}
		if receivedKey.Package.Arch != "aarch64" {
			t.Errorf("key.Package.Arch = %q, want %q", receivedKey.Package.Arch, "aarch64")
		}
	})

	t.Run("valid key with friendly name repo path", func(t *testing.T) {
		key := "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk"
		var receivedKey *apkurl.Key
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			receivedKey = key
			return nil
		}))

		err := r.Reconcile(context.Background(), key)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		if receivedKey.Host != "packages.wolfi.dev" {
			t.Errorf("key.Host = %q, want %q", receivedKey.Host, "packages.wolfi.dev")
		}
		if receivedKey.RepoPath != "os" {
			t.Errorf("key.RepoPath = %q, want %q", receivedKey.RepoPath, "os")
		}
	})

	t.Run("invalid key - bad format", func(t *testing.T) {
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			t.Error("reconcileFunc should not be called for invalid key")
			return nil
		}))

		err := r.Reconcile(context.Background(), "not-a-valid-key")
		if err == nil {
			t.Fatal("Reconcile() should error for invalid key")
		}

		// Should be a non-retriable error
		if details := workqueue.GetNonRetriableDetails(err); details == nil {
			t.Error("invalid key should return non-retriable error")
		}
	})

	t.Run("reconciler returns error", func(t *testing.T) {
		expectedErr := errors.New("reconciler failed")
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			return expectedErr
		}))

		err := r.Reconcile(context.Background(), validKey)
		if err == nil {
			t.Fatal("Reconcile() should propagate reconciler error")
		}
		if !errors.Is(err, expectedErr) {
			t.Errorf("Reconcile() error = %v, want %v", err, expectedErr)
		}
	})
}

func TestProcess(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			return nil
		}))

		resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{
			Key:      validKey,
			Priority: 1,
		})

		if err != nil {
			t.Fatalf("Process() error = %v", err)
		}
		if resp == nil {
			t.Fatal("Process() returned nil response")
		}
		if resp.RequeueAfterSeconds != 0 {
			t.Errorf("Process() RequeueAfterSeconds = %d, want 0", resp.RequeueAfterSeconds)
		}
	})

	t.Run("requeue after", func(t *testing.T) {
		requeueDuration := 30 * time.Second
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			return workqueue.RequeueAfter(requeueDuration)
		}))

		resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{
			Key:      validKey,
			Priority: 1,
		})

		if err != nil {
			t.Fatalf("Process() error = %v", err)
		}
		if resp == nil {
			t.Fatal("Process() returned nil response")
		}
		if resp.RequeueAfterSeconds != int64(requeueDuration.Seconds()) {
			t.Errorf("Process() RequeueAfterSeconds = %d, want %d", resp.RequeueAfterSeconds, int64(requeueDuration.Seconds()))
		}
	})

	t.Run("non-retriable error", func(t *testing.T) {
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			return workqueue.NonRetriableError(errors.New("permanent failure"), "test reason")
		}))

		resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{
			Key:      validKey,
			Priority: 1,
		})

		// Non-retriable errors should not return an error from Process
		if err != nil {
			t.Fatalf("Process() error = %v, want nil for non-retriable", err)
		}
		if resp == nil {
			t.Fatal("Process() returned nil response")
		}
		if resp.RequeueAfterSeconds != 0 {
			t.Errorf("Process() RequeueAfterSeconds = %d, want 0", resp.RequeueAfterSeconds)
		}
	})

	t.Run("non-retriable error from invalid key", func(t *testing.T) {
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			return nil
		}))

		resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{
			Key:      "invalid-key",
			Priority: 1,
		})

		// Non-retriable errors from parsing should not return an error
		if err != nil {
			t.Fatalf("Process() error = %v, want nil for non-retriable", err)
		}
		if resp == nil {
			t.Fatal("Process() returned nil response")
		}
	})

	t.Run("transient error", func(t *testing.T) {
		transientErr := errors.New("temporary failure")
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			return transientErr
		}))

		resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{
			Key:      validKey,
			Priority: 1,
		})

		// Transient errors should return an error from Process
		if err == nil {
			t.Fatal("Process() should return error for transient failure")
		}
		if !errors.Is(err, transientErr) {
			t.Errorf("Process() error = %v, want %v", err, transientErr)
		}
		if resp != nil {
			t.Errorf("Process() response = %v, want nil for transient error", resp)
		}
	})

	t.Run("context passed to reconciler", func(t *testing.T) {
		type ctxKey struct{}
		ctxValue := "test-value"

		var receivedCtx context.Context
		r := New(WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
			receivedCtx = ctx
			return nil
		}))

		ctx := context.WithValue(context.Background(), ctxKey{}, ctxValue)
		_, err := r.Process(ctx, &workqueue.ProcessRequest{
			Key:      validKey,
			Priority: 1,
		})

		if err != nil {
			t.Fatalf("Process() error = %v", err)
		}
		if receivedCtx.Value(ctxKey{}) != ctxValue {
			t.Error("context was not passed to reconciler")
		}
	})
}

func TestWithReconciler(t *testing.T) {
	called := false
	fn := func(ctx context.Context, key *apkurl.Key) error {
		called = true
		return nil
	}

	r := &Reconciler{}
	opt := WithReconciler(fn)
	opt(r)

	if r.reconcileFunc == nil {
		t.Fatal("WithReconciler did not set reconcileFunc")
	}

	// Call it to verify it's the right function
	_ = r.reconcileFunc(context.Background(), &apkurl.Key{})
	if !called {
		t.Error("WithReconciler set wrong function")
	}
}
