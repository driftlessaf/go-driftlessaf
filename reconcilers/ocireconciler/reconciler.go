/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package ocireconciler

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-containerregistry/pkg/name"
)

// Reconciler provides a workqueue processor for OCI digests.
type Reconciler struct {
	workqueue.UnimplementedWorkqueueServiceServer

	reconcileFunc ReconcilerFunc
	nameOpts      []name.Option
}

// New constructs a Reconciler with the provided options.
func New(opts ...Option) *Reconciler {
	r := &Reconciler{}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Reconcile resolves the digest key and invokes the configured reconciliation func.
func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	if r.reconcileFunc == nil {
		return errors.New("no reconciler configured")
	}
	digest, err := name.NewDigest(key, r.nameOpts...)
	if err != nil {
		return workqueue.NonRetriableError(fmt.Errorf("parsing digest %q: %w", key, err), "invalid digest key")
	}
	return r.reconcileFunc(ctx, digest)
}

// Process implements the WorkqueueService.
func (r *Reconciler) Process(ctx context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	clog.InfoContextf(ctx, "Processing OCI digest: %s (priority: %d)", req.Key, req.Priority)

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

	clog.InfoContextf(ctx, "Successfully reconciled OCI digest: %s", req.Key)
	return &workqueue.ProcessResponse{}, nil
}
