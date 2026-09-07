/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package apkreconciler

import (
	"context"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/breaker"
	"chainguard.dev/driftlessaf/reconcilers/apkreconciler/apkurl"
	"chainguard.dev/driftlessaf/workqueue"
)

func TestProcessBreakerErrorRequeuesWithFloor(t *testing.T) {
	const delay = 5 * time.Minute
	r := New(WithReconciler(func(context.Context, *apkurl.Key) error {
		return &breaker.Error{Key: "rekor.example", RetryAfter: delay}
	}))

	resp, err := r.Process(t.Context(), &workqueue.ProcessRequest{Key: validKey})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if got, want := resp.RequeueAfterSeconds, int64(delay.Seconds()); got != want {
		t.Errorf("RequeueAfterSeconds = %d, want %d", got, want)
	}
	if !resp.RequeueFloor {
		t.Error("RequeueFloor = false, want true")
	}
}
