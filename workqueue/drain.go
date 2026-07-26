/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Note that these are variables, so that they can be modified by tests and
// made flags in binary entrypoints.
var (
	// DrainBackstop bounds how long DrainInterceptor waits, once draining
	// begins, for an in-flight handler to unwind cooperatively before
	// abandoning it and answering on its behalf. It must comfortably fit
	// inside the platform's SIGTERM grace (10s on Cloud Run) together with
	// the response flush.
	DrainBackstop = 3 * time.Second

	// DrainRequeueDelay is the minimum requeue delay the interceptor
	// answers with while draining; DrainRequeueJitter adds a random extra
	// delay in [0, jitter) so a retiring instance's in-flight keys do not
	// all come back at once.
	DrainRequeueDelay  = 15 * time.Second
	DrainRequeueJitter = 30 * time.Second
)

// errDraining marks request contexts canceled by DrainInterceptor because
// the server is shutting down, distinguishing them from client-initiated
// cancellations.
var errDraining = errors.New("workqueue: server draining")

// drainRequeueResponse builds the response the interceptor answers with on
// behalf of work interrupted by shutdown: a plain requeue-after, which the
// dispatcher applies with Options.Delay semantics — resetting the key's
// attempt count. An orderly instance retirement is infrastructure's doing,
// so it must not consume the key's dead-letter budget; hard kills (OOM,
// SIGKILL without grace) never reach this path and still burn an attempt,
// which keeps genuinely poisonous keys terminating.
func drainRequeueResponse() *ProcessResponse {
	delay := DrainRequeueDelay
	if DrainRequeueJitter > 0 {
		delay += rand.N(DrainRequeueJitter) //nolint:gosec // G404: jitter, not security-sensitive
	}
	return &ProcessResponse{RequeueAfterSeconds: int64(delay.Seconds())}
}

// DrainInterceptor returns a unary server interceptor that converts orderly
// shutdown into requeues instead of severed callbacks.
//
// gRPC request contexts are created from the transport stream and
// deliberately do not derive from any application context, so a top-level
// cancellation (e.g. signal.NotifyContext observing SIGTERM) is invisible
// to in-flight reconciles: the process keeps working until the platform's
// grace expires and SIGKILL severs every connection, which the dispatcher
// records as a failed attempt against each in-flight key. This interceptor
// bridges the two cancellation domains per request:
//
//   - a Process call arriving while drain is already canceled is answered
//     immediately with a requeue, without starting work;
//   - an in-flight Process call has its request context canceled when drain
//     fires, and its cooperative unwind (or, past DrainBackstop, the
//     interceptor answering on its behalf) is translated into a requeue
//     response rather than an error;
//   - a handler that completes successfully during the drain window keeps
//     its real response.
//
// Because the requeue is delivered as RequeueAfterSeconds, the dispatcher
// resets the key's attempt count: announced shutdowns stop consuming
// dead-letter budget entirely, while unannounced deaths keep their existing
// (attempt-consuming) semantics.
//
// Wire it with the recovery interceptor after (inside) it, so handler
// panics remain the recovery interceptor's to translate:
//
//	grpc.ChainUnaryInterceptor(
//	    ...,
//	    workqueue.DrainInterceptor(ctx), // ctx canceled on SIGTERM
//	    recovery.UnaryServerInterceptor(),
//	)
//
// Methods other than WorkqueueService/Process pass through untouched.
func DrainInterceptor(drain context.Context) grpc.UnaryServerInterceptor {
	processMethod := fmt.Sprintf("/%s/Process", WorkqueueService_ServiceDesc.ServiceName)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod != processMethod {
			return handler(ctx, req)
		}
		if drain.Err() != nil {
			return drainRequeueResponse(), nil
		}

		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		stop := context.AfterFunc(drain, func() { cancel(errDraining) })
		defer stop()

		type result struct {
			resp any
			err  error
		}
		done := make(chan result, 1)
		go func() {
			// The handler is abandoned if it outlives DrainBackstop; a
			// panic on the abandoned goroutine must not crash the process
			// out from under the other in-flight requests, so translate it
			// the way the (inner) recovery interceptor would have.
			defer func() {
				if p := recover(); p != nil {
					done <- result{nil, status.Errorf(codes.Internal, "handler panic: %v", p)}
				}
			}()
			resp, err := handler(ctx, req)
			done <- result{resp, err}
		}()

		finish := func(r result) (any, error) {
			// Work that failed because drain canceled its context is not a
			// verdict about the key; answer with a requeue. Work that
			// completed (or failed on its own terms before the
			// cancellation landed) keeps its real outcome.
			if r.err != nil && errors.Is(context.Cause(ctx), errDraining) {
				return drainRequeueResponse(), nil
			}
			return r.resp, r.err
		}

		select {
		case r := <-done:
			return finish(r)
		case <-drain.Done():
			backstop := time.NewTimer(DrainBackstop)
			defer backstop.Stop()
			select {
			case r := <-done:
				return finish(r)
			case <-backstop.C:
				return drainRequeueResponse(), nil
			}
		}
	}
}
