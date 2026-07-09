/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package ocireconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/breaker"
	"chainguard.dev/driftlessaf/reconcilers/transient"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-containerregistry/pkg/name"
)

// Transient failures are requeued with jitter so keys that failed together
// don't come back in lockstep.
const (
	transientRequeueDelay  = 10 * time.Second
	transientRequeueJitter = 50 * time.Second
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
//
// Errors wrapping a *breaker.Error (transient host failures reported by
// breaker.Transport) requeue with the breaker's backoff, floored so periodic
// re-enqueues can't undercut it, and never dead-letter.
func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	if r.reconcileFunc == nil {
		return errors.New("no reconciler configured")
	}
	digest, err := name.NewDigest(key, r.nameOpts...)
	if err != nil {
		return workqueue.NonRetriableError(fmt.Errorf("parsing digest %q: %w", key, err), "invalid digest key")
	}
	var berr *breaker.Error
	rerr := r.reconcileFunc(ctx, digest)
	switch {
	case rerr == nil:
		return nil
	case errors.As(rerr, &berr):
		clog.WarnContextf(ctx, "Transient failure reconciling %s, requeueing after %s: %v", key, berr.RetryAfter, rerr)
		return workqueue.RequeueNotBefore(berr.RetryAfter)
	case transient.Is(rerr):
		clog.WarnContextf(ctx, "Transient failure, requeueing with jitter: %v", rerr)
		return workqueue.RequeueAfterWithJitter(transientRequeueDelay, transientRequeueJitter)
	default:
		return rerr
	}
}

// Process implements the WorkqueueService.
func (r *Reconciler) Process(ctx context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	clog.InfoContextf(ctx, "Processing OCI digest: %s (priority: %d)", req.Key, req.Priority)

	err := r.Reconcile(ctx, req.Key)
	if err != nil {
		if delay, floor, ok := workqueue.GetRequeueOptions(err); ok {
			clog.InfoContextf(ctx, "Reconciliation requested requeue after %v (floor=%t) for key: %s", delay, floor, req.Key)
			return &workqueue.ProcessResponse{RequeueAfterSeconds: int64(delay.Seconds()), RequeueFloor: floor}, nil
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
