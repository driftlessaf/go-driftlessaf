/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package apkreconciler

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/apkreconciler/apkurl"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
)

// Reconciler provides a workqueue processor for APK keys.
type Reconciler struct {
	workqueue.UnimplementedWorkqueueServiceServer

	reconcileFunc ReconcilerFunc
}

// New constructs a Reconciler with the provided options.
func New(opts ...Option) *Reconciler {
	r := &Reconciler{}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Reconcile parses the APK key and invokes the configured reconciliation func.
// Keys are of the form "{host}/{uidp}/{arch}/{package}-{version}.apk" and do
// not include the scheme. Use key.URL() to get the full HTTPS URL for fetching.
func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	if r.reconcileFunc == nil {
		return errors.New("no reconciler configured")
	}

	// Parse the APK key into its components
	apkKey, err := apkurl.Parse(key)
	if err != nil {
		return workqueue.NonRetriableError(fmt.Errorf("parsing APK key %q: %w", key, err), "invalid APK key")
	}

	return r.reconcileFunc(ctx, apkKey)
}

// Process implements the WorkqueueService.
func (r *Reconciler) Process(ctx context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	clog.InfoContextf(ctx, "Processing APK URL: %s (priority: %d)", req.Key, req.Priority)

	err := r.Reconcile(ctx, req.Key)
	if err != nil {
		if delay, ok := workqueue.GetRequeueDelay(err); ok {
			clog.InfoContextf(ctx, "Reconciliation requested requeue after %v for key: %s", delay, req.Key)
			return &workqueue.ProcessResponse{RequeueAfterSeconds: int64(delay.Seconds())}, nil
		}

		if details := workqueue.GetNonRetriableDetails(err); details != nil {
			clog.WarnContextf(ctx, "Reconciliation failed with non-retriable error for key %s: %v (reason: %s)", req.Key, err, details.Message)
			return &workqueue.ProcessResponse{}, nil
		}

		clog.ErrorContextf(ctx, "Reconciliation failed for key %s: %v", req.Key, err)
		return nil, err
	}

	clog.InfoContextf(ctx, "Successfully reconciled APK URL: %s", req.Key)
	return &workqueue.ProcessResponse{}, nil
}
