/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	"chainguard.dev/driftlessaf/workqueue"
)

// defaultDispatchPeriod is the pass-admission interval used when
// WithDispatchPeriod is not supplied.
const defaultDispatchPeriod = time.Second

func Handler(wq workqueue.Interface, concurrency, batchSize int, f Callback, maxRetry int, opts ...Option) http.Handler {
	period := applyOptions(opts).dispatchPeriod
	if period <= 0 {
		period = defaultDispatchPeriod
	}
	return &handler{
		// burst 1: pass admissions are at least one period apart.
		limiter:     rate.NewLimiter(rate.Every(period), 1),
		wq:          wq,
		concurrency: concurrency,
		batchSize:   batchSize,
		f:           f,
		maxRetry:    maxRetry,
		opts:        opts,
	}
}

type handler struct {
	limiter *rate.Limiter
	// repassPending is set while one shed trigger waits for the next
	// admission, so shed triggers cannot pile up waiters.
	repassPending atomic.Bool
	// repassing is set while one request, having seen a batch-capped pass, runs
	// the re-pass loop.
	repassing atomic.Bool

	wq          workqueue.Interface
	concurrency int
	batchSize   int
	f           Callback
	maxRetry    int
	opts        []Option
}

var _ http.Handler = (*handler)(nil)

// ServeHTTP implements http.Handler
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := requestTraceContext(r)

	// Admit at most one dispatch pass per period. A trigger beyond that is
	// acknowledged and dropped, except that one shed trigger at a time waits
	// for the next admission and runs a pass then: the pass in flight may
	// have listed the queue before the shed trigger's work arrived, and the
	// events that used to restart it are no longer published. Passes are
	// deliberately unbounded in flight: a slow pass over a deep queue must not
	// stop a full sweep from launching each period.
	if !h.limiter.Allow() {
		countTrigger(triggerShed)
		if !h.repassPending.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusOK)
			return
		}
		err := h.limiter.Wait(ctx)
		h.repassPending.Store(false)
		if err != nil {
			// The request ended first; the periodic trigger is the backstop.
			w.WriteHeader(http.StatusOK)
			return
		}
		countTrigger(triggerRepass)
	} else {
		countTrigger(triggerDispatched)
	}

	// A pass that stops at its batch cap with slots open and eligible work
	// queued would leave that capacity idle until another trigger arrived, so
	// follow it with one pass per admission for as long as that holds. Each
	// extra pass waits for a limiter slot, and the loop ends as soon as a pass
	// is limited by slots or by the queue instead of by the batch, or none of
	// its claims succeeded (an outage would otherwise repeat every period).
	// Only one request per instance runs this loop at a time; any other
	// request runs its single pass. That bounds the keys pinned to a
	// request to its own batch, except for the one looping request, whose
	// pins are bounded by the open slots. The passes share that request's
	// deadline, where the batches of separate triggers would each have had
	// their own: the keys of later passes are cancelled with the first pass's
	// if the request times out or its sender hangs up.
	loop := false
	var futures []Future
	for {
		future, capped, settle := handleAsync(ctx, h.wq, h.concurrency, h.batchSize, h.f, h.maxRetry, h.opts...)
		futures = append(futures, future)
		if !capped {
			break
		}
		// Let this pass's claims land first: enumerating ahead of them would
		// count too few in-progress keys and launch past the open slots. A
		// pass whose claims all failed changed nothing, so repeating it would
		// only repeat the failure.
		if settle() == 0 {
			break
		}
		// Take the loop only once a pass shows it is needed and productive: a
		// request still enumerating or claiming, or one that found nothing to
		// do, must not hold it against a request that found a backlog.
		if !loop {
			if loop = h.repassing.CompareAndSwap(false, true); !loop {
				break
			}
		}
		if h.limiter.Wait(ctx) != nil {
			break
		}
		countTrigger(triggerRepass)
	}
	if loop {
		// Free the loop for later triggers before waiting on the work: the
		// keys this request launched hold their slots until they finish.
		h.repassing.Store(false)
	}

	var firstErr error
	for _, future := range futures {
		if err := future(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		http.Error(w, firstErr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func requestTraceContext(r *http.Request) context.Context {
	if oteltrace.SpanContextFromContext(r.Context()).IsValid() {
		return r.Context()
	}
	return propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
}
