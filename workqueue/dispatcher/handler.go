/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"net/http"
	"time"

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
	// Admit at most one dispatch pass per period. Triggers beyond that are
	// acknowledged and dropped — the admitted passes already cover the
	// queue, and the workqueue's periodic triggers provide the follow-up
	// passes. Passes are deliberately unbounded in flight: a slow pass over
	// a deep queue must not stop a full sweep from launching each period.
	if !h.limiter.Allow() {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := HandleAsync(r.Context(), h.wq, h.concurrency, h.batchSize, h.f, h.maxRetry, h.opts...)(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
