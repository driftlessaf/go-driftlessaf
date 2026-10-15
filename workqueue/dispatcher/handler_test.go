/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	oteltrace "go.opentelemetry.io/otel/trace"

	"chainguard.dev/driftlessaf/workqueue"
)

func randomSpanContext(t *testing.T) oteltrace.SpanContext {
	t.Helper()

	for {
		var traceID oteltrace.TraceID
		if _, err := cryptorand.Read(traceID[:]); err != nil {
			t.Fatalf("generate trace ID: %v", err)
		}
		var spanID oteltrace.SpanID
		if _, err := cryptorand.Read(spanID[:]); err != nil {
			t.Fatalf("generate span ID: %v", err)
		}
		if spanContext := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID: traceID,
			SpanID:  spanID,
		}); spanContext.IsValid() {
			return spanContext
		}
	}
}

func TestRequestTraceContext(t *testing.T) {
	headerSpanContext := randomSpanContext(t)
	activeSpanContext := randomSpanContext(t)
	tests := []struct {
		name string
		ctx  context.Context
		want oteltrace.SpanContext
	}{{
		name: "extracts header without active context",
		ctx:  t.Context(),
		want: headerSpanContext,
	}, {
		name: "preserves active context",
		ctx:  oteltrace.ContextWithSpanContext(t.Context(), activeSpanContext),
		want: activeSpanContext,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(tt.ctx, http.MethodPost, "/", nil)
			req.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", headerSpanContext.TraceID(), headerSpanContext.SpanID()))
			got := oteltrace.SpanContextFromContext(requestTraceContext(req))
			if got.TraceID() != tt.want.TraceID() {
				t.Errorf("trace ID: got = %q, want = %q", got.TraceID(), tt.want.TraceID())
			}
			if got.SpanID() != tt.want.SpanID() {
				t.Errorf("span ID: got = %q, want = %q", got.SpanID(), tt.want.SpanID())
			}
		})
	}
}

// triggerCount reads the current value of the trigger counter for the given
// result, so tests can assert deltas against the process-global metric.
func triggerCount(t *testing.T, result string) float64 {
	t.Helper()
	c, err := mTriggers.GetMetricWithLabelValues(metricServiceName, metricRevisionName, result)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", result, err)
	}
	return testutil.ToFloat64(c)
}

// blockingQueue is a workqueue whose Enumerate blocks until released,
// counting calls, so tests can hold dispatch passes in flight.
type blockingQueue struct {
	enumerations atomic.Int64
	release      chan struct{}
}

var _ workqueue.Interface = (*blockingQueue)(nil)

func (b *blockingQueue) Identity() string { return "" }

func (b *blockingQueue) Queue(context.Context, string, workqueue.Options) error {
	return nil
}

func (b *blockingQueue) Enumerate(ctx context.Context) ([]workqueue.ObservedInProgressKey, []workqueue.QueuedKey, []workqueue.DeadLetteredKey, error) {
	b.enumerations.Add(1)
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		}
	}
	return nil, nil, nil, nil
}

func (b *blockingQueue) Get(context.Context, string) (*workqueue.KeyState, error) {
	return &workqueue.KeyState{}, nil
}

func TestHandler_DispatchesEmptyQueue(t *testing.T) {
	q := &blockingQueue{}
	h := Handler(q, 1, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
		WithDispatchPeriod(time.Nanosecond))
	dispatched := triggerCount(t, triggerDispatched)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Errorf("status: got = %d, want = %d", got, want)
	}
	if got := q.enumerations.Load(); got != 1 {
		t.Errorf("enumerations: got = %d, want = 1", got)
	}
	if got, want := triggerCount(t, triggerDispatched)-dispatched, 1.0; got != want {
		t.Errorf("dispatched triggers: got = %v, want = %v", got, want)
	}
}

func TestHandler_ShedsWithinPeriod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := &blockingQueue{}
		h := Handler(q, 1, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
			WithDispatchPeriod(time.Hour))
		dispatched := triggerCount(t, triggerDispatched)
		shed := triggerCount(t, triggerShed)

		// The first trigger consumes the period's admission.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
		if got := q.enumerations.Load(); got != 1 {
			t.Fatalf("enumerations: got = %d, want = 1", got)
		}

		// The first shed trigger waits for the next admission, which is an hour
		// away, so it is parked until its request ends.
		waiterCtx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		waiterDone := make(chan int, 1)
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(waiterCtx, "POST", "/", nil))
			waiterDone <- rec.Code
		}()
		// Wait returns once the waiter is parked on the limiter, with no timer.
		synctest.Wait()
		if !h.(*handler).repassPending.Load() {
			t.Fatal("no shed trigger became the pending re-pass")
		}

		// Further triggers within the period shed promptly with 200 and do not
		// dispatch: only one shed trigger waits at a time.
		for i := range 3 {
			start := time.Now()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
			if got, want := rec.Code, http.StatusOK; got != want {
				t.Errorf("shed %d status: got = %d, want = %d", i, got, want)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("shed %d latency: got = %v, want prompt", i, elapsed)
			}
		}

		// Ending the waiting request acknowledges it without a pass; the periodic
		// trigger is the backstop.
		cancel()
		if got, want := <-waiterDone, http.StatusOK; got != want {
			t.Errorf("waiter status: got = %d, want = %d", got, want)
		}
		if h.(*handler).repassPending.Load() {
			t.Error("repassPending: got = true after the waiter ended, want = false")
		}
		if got := q.enumerations.Load(); got != 1 {
			t.Errorf("enumerations after sheds: got = %d, want = 1", got)
		}
		if got, want := triggerCount(t, triggerDispatched)-dispatched, 1.0; got != want {
			t.Errorf("dispatched triggers: got = %v, want = %v", got, want)
		}
		if got, want := triggerCount(t, triggerShed)-shed, 4.0; got != want {
			t.Errorf("shed triggers: got = %v, want = %v", got, want)
		}
	})
}

func TestHandler_PassesOverlap(t *testing.T) {
	const passes = 3
	q := &blockingQueue{release: make(chan struct{})}
	h := Handler(q, 1, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
		WithDispatchPeriod(time.Nanosecond))

	// Launch passes one at a time, each admitted while the prior ones are
	// still blocked inside Enumerate: sweeps must stack rather than being
	// bounded in flight.
	done := make(chan struct{}, passes)
	for i := range passes {
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
			done <- struct{}{}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for q.enumerations.Load() < int64(i+1) {
			if time.Now().After(deadline) {
				t.Fatalf("enumerations: got = %d, want = %d before deadline", q.enumerations.Load(), i+1)
			}
			time.Sleep(time.Millisecond)
		}
	}

	close(q.release)
	for range passes {
		<-done
	}
	if got, want := q.enumerations.Load(), int64(passes); got != want {
		t.Errorf("enumerations: got = %d, want = %d", got, want)
	}
}

// fillQueue is a stateful workqueue: Start moves a key from queued to
// in-progress, and the callback holds it there until release is closed, which
// models long-running work. Enumerate counts passes.
type fillQueue struct {
	mu      sync.Mutex
	queued  []workqueue.QueuedKey
	wip     []workqueue.ObservedInProgressKey
	passes  int
	started atomic.Int64
	release chan struct{}
	// holdFirst, when set, parks the first Enumerate on an empty listing until
	// it is closed, as a slow listing that finds nothing would.
	holdFirst chan struct{}
	// firstPass, when set, is what the first Enumerate lists instead of the
	// queue.
	firstPass []workqueue.QueuedKey
	// failStarts makes every claim fail, as during a storage outage.
	failStarts atomic.Bool
	allReady   chan struct{}
	want       int64
}

var _ workqueue.Interface = (*fillQueue)(nil)

type fillKey struct {
	*mockKey
	q *fillQueue
	// gate, when set, blocks Start until closed and then fails it.
	gate chan struct{}
}

func (k *fillKey) Start(ctx context.Context) (workqueue.OwnedInProgressKey, error) {
	if k.gate != nil {
		<-k.gate
		return nil, errors.New("storage unavailable")
	}
	if k.q.failStarts.Load() {
		return nil, errors.New("storage unavailable")
	}
	k.q.mu.Lock()
	defer k.q.mu.Unlock()
	for _, w := range k.q.wip {
		if w.Name() == k.name {
			return nil, fmt.Errorf("key %q already in progress", k.name)
		}
	}
	k.q.queued = slices.DeleteFunc(k.q.queued, func(x workqueue.QueuedKey) bool { return x.Name() == k.name })
	k.q.wip = append(k.q.wip, k.mockKey)
	if k.q.started.Add(1) == k.q.want {
		close(k.q.allReady)
	}
	return k.mockKey.Start(ctx)
}

func newFillQueue(keys int, want int64) *fillQueue {
	q := &fillQueue{release: make(chan struct{}), allReady: make(chan struct{}), want: want}
	for i := range keys {
		q.queued = append(q.queued, &fillKey{mockKey: &mockKey{name: fmt.Sprintf("key-%03d", i)}, q: q})
	}
	return q
}

func (q *fillQueue) Identity() string { return "" }

func (q *fillQueue) Enumerate(context.Context) ([]workqueue.ObservedInProgressKey, []workqueue.QueuedKey, []workqueue.DeadLetteredKey, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.passes++
	if q.passes == 1 && q.holdFirst != nil {
		q.mu.Unlock()
		<-q.holdFirst
		q.mu.Lock()
		return nil, nil, nil, nil
	}
	if q.passes == 1 && q.firstPass != nil {
		return nil, slices.Clone(q.firstPass), nil, nil
	}
	return slices.Clone(q.wip), slices.Clone(q.queued), nil, nil
}

func (q *fillQueue) Queue(context.Context, string, workqueue.Options) error { return nil }

func (q *fillQueue) Get(context.Context, string) (*workqueue.KeyState, error) {
	return &workqueue.KeyState{}, nil
}

func (q *fillQueue) passCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.passes
}

// releaseOnExit closes ch when the test ends, if it is still open, so a failed
// assertion does not leave goroutines blocked on it and deadlock the bubble.
func releaseOnExit(t *testing.T, ch chan struct{}) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-ch:
		default:
			close(ch)
		}
	})
}

// serve sends one trigger, returning a channel that yields the response code
// once the request, which waits for the launched work, completes.
func serve(t *testing.T, h http.Handler) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
		done <- rec.Code
	}()
	return done
}

// TestHandler_BatchCappedBacklogFillsSlots reproduces a backlog that becomes
// eligible at once on long-running work: with 100 open slots and a batch of 10,
// one trigger must keep passing, one per limiter slot, until every slot is
// filled. Without the re-pass a single trigger launches one batch and the
// remaining slots wait for the next trigger.
func TestHandler_BatchCappedBacklogFillsSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			slots  = 100
			batch  = 10
			period = 5 * time.Millisecond
		)
		q := newFillQueue(250, slots)
		releaseOnExit(t, q.release)
		work := func(context.Context, string, workqueue.Options) error {
			<-q.release
			return nil
		}
		h := Handler(q, slots, batch, work, 0, WithDispatchPeriod(period))
		repass := triggerCount(t, triggerRepass)

		start := time.Now()
		done := serve(t, h)
		select {
		case <-q.allReady:
		case <-time.After(10 * time.Second):
			t.Fatalf("started: got = %d, want = %d from one trigger (passes: %d)", q.started.Load(), slots, q.passCount())
		}
		elapsed := time.Since(start)

		// Join the request so the pass and trigger counts are final. Every claim
		// lands before the next pass enumerates, so each pass sees the slots the
		// last one filled: slots/batch passes launch every key, and the last of
		// them, finding no slot left over, ends the loop.
		close(q.release)
		if got, want := <-done, http.StatusOK; got != want {
			t.Errorf("status: got = %d, want = %d", got, want)
		}
		if got, want := q.passCount(), slots/batch; got != want {
			t.Errorf("passes: got = %d, want = %d", got, want)
		}
		if got, want := triggerCount(t, triggerRepass)-repass, float64(slots/batch-1); got != want {
			t.Errorf("repass triggers: got = %v, want = %v", got, want)
		}
		// No pass launched past the open slots: every claim was a distinct key.
		q.mu.Lock()
		names := map[string]struct{}{}
		for _, w := range q.wip {
			names[w.Name()] = struct{}{}
		}
		if got, want := len(q.wip), slots; got != want || len(names) != want {
			t.Errorf("in-progress keys: got = %d (%d distinct), want = %d distinct", got, len(names), want)
		}
		q.mu.Unlock()
		// One pass per limiter slot: on virtual time the fill takes exactly one
		// period per re-pass, far faster than a per-minute cron would deliver.
		if want := (slots/batch - 1) * period; elapsed != want {
			t.Errorf("fill time: got = %v, want = %v", elapsed, want)
		}
		t.Logf("filled %d slots in %d passes over %v", slots, slots/batch, elapsed)
	})
}

func TestHandler_NoRepassWithoutOpenSlotsOrWork(t *testing.T) {
	tests := []struct {
		name        string
		keys        int
		concurrency int
		batch       int
		wantPasses  int
		wantStarted int64
	}{{
		name:        "slots filled by the batch",
		keys:        50,
		concurrency: 10,
		batch:       10,
		wantPasses:  1,
		wantStarted: 10,
	}, {
		name:        "batch not larger than the queue",
		keys:        7,
		concurrency: 100,
		batch:       10,
		wantPasses:  1,
		wantStarted: 7,
	}, {
		name:        "batch exactly drains the queue",
		keys:        10,
		concurrency: 100,
		batch:       10,
		wantPasses:  1,
		wantStarted: 10,
	}, {
		name:        "empty queue",
		keys:        0,
		concurrency: 100,
		batch:       10,
		wantPasses:  1,
		wantStarted: 0,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := newFillQueue(tt.keys, -1)
				h := Handler(q, tt.concurrency, tt.batch, func(context.Context, string, workqueue.Options) error {
					<-q.release
					return nil
				}, 0, WithDispatchPeriod(time.Millisecond))
				repass := triggerCount(t, triggerRepass)

				// Joining the request, rather than sleeping, means a loop that
				// failed to end would hang here instead of passing unnoticed.
				close(q.release)
				if got, want := <-serve(t, h), http.StatusOK; got != want {
					t.Errorf("status: got = %d, want = %d", got, want)
				}
				if got := q.passCount(); got != tt.wantPasses {
					t.Errorf("passes: got = %d, want = %d", got, tt.wantPasses)
				}
				if got := q.started.Load(); got != tt.wantStarted {
					t.Errorf("started: got = %d, want = %d", got, tt.wantStarted)
				}
				if got := triggerCount(t, triggerRepass) - repass; got != 0 {
					t.Errorf("repass triggers: got = %v, want = 0", got)
				}
			})
		})
	}
}

// TestHandler_ShedTriggerRunsOneRepass asserts a shed trigger is followed by a
// pass at the next limiter slot, and that a burst of sheds adds only one.
func TestHandler_ShedTriggerRunsOneRepass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const period = 50 * time.Millisecond
		q := newFillQueue(0, -1)
		close(q.release)
		h := Handler(q, 10, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
			WithDispatchPeriod(period))
		repass := triggerCount(t, triggerRepass)

		// The first trigger runs a pass; a burst right behind it is shed.
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
				if got, want := rec.Code, http.StatusOK; got != want {
					t.Errorf("status: got = %d, want = %d", got, want)
				}
			})
		}
		wg.Wait()

		if got, want := q.passCount(), 2; got != want {
			t.Errorf("passes: got = %d, want = %d (one admitted, one re-pass for the shed burst)", got, want)
		}
		if got, want := triggerCount(t, triggerRepass)-repass, 1.0; got != want {
			t.Errorf("repass triggers: got = %v, want = %v", got, want)
		}
	})
}

// requestWithin sends one trigger whose request ends after d, so a loop that
// fails to terminate shows up as extra passes instead of hanging the test.
func requestWithin(t *testing.T, h http.Handler, d time.Duration) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "POST", "/", nil))
	return rec.Code
}

// TestHandler_NoRepassWhenEveryClaimFails asserts a batch-capped pass whose
// claims all fail is not repeated every period: nothing changed, so the next
// pass would see the same open slots and fail the same way.
func TestHandler_NoRepassWhenEveryClaimFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFillQueue(250, -1)
		q.failStarts.Store(true)
		close(q.release)
		h := Handler(q, 100, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
			WithDispatchPeriod(time.Millisecond))
		repass := triggerCount(t, triggerRepass)

		// Far longer than the period, so a loop that kept going would run
		// hundreds of passes.
		if got, want := requestWithin(t, h, 500*time.Millisecond), http.StatusOK; got != want {
			t.Errorf("status: got = %d, want = %d", got, want)
		}
		if got, want := q.passCount(), 1; got != want {
			t.Errorf("passes: got = %d, want = %d", got, want)
		}
		if got := triggerCount(t, triggerRepass) - repass; got != 0 {
			t.Errorf("repass triggers: got = %v, want = 0", got)
		}
	})
}

// TestHandler_OnlyOneRequestRunsTheRepassLoop asserts that while one request
// runs the re-pass loop, another admitted request runs its single pass and no
// more, so requests cannot pile up each pinning keys across several passes.
func TestHandler_OnlyOneRequestRunsTheRepassLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFillQueue(250, -1)
		h := Handler(q, 100, 10, func(context.Context, string, workqueue.Options) error {
			<-q.release
			return nil
		}, 0, WithDispatchPeriod(time.Millisecond))
		releaseOnExit(t, q.release)
		h.(*handler).repassing.Store(true) // another request is looping

		done := serve(t, h)
		// Every goroutine is now blocked: the batch's work on release, and the
		// request on its futures. Time is virtual, so once release is closed the
		// limiter's slots pass instantly, and a request that took the loop anyway
		// would run further passes before it returned.
		synctest.Wait()
		if got, want := q.started.Load(), int64(10); got != want {
			t.Fatalf("started: got = %d, want = %d", got, want)
		}
		close(q.release)
		if got, want := <-done, http.StatusOK; got != want {
			t.Errorf("status: got = %d, want = %d", got, want)
		}
		if got, want := q.passCount(), 1; got != want {
			t.Errorf("passes: got = %d, want = %d", got, want)
		}
		if got, want := q.started.Load(), int64(10); got != want {
			t.Errorf("started: got = %d, want = %d (one batch)", got, want)
		}
		if !h.(*handler).repassing.Load() {
			t.Error("repassing: got = false, want the other request's flag left set")
		}
	})
}

// TestHandler_RepassLoopFlagIsReleased asserts the flag is free again once the
// looping request has finished its passes, even while its work still runs.
func TestHandler_RepassLoopFlagIsReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFillQueue(250, 100)
		h := Handler(q, 100, 10, func(context.Context, string, workqueue.Options) error {
			<-q.release
			return nil
		}, 0, WithDispatchPeriod(time.Millisecond))
		releaseOnExit(t, q.release)
		done := serve(t, h)
		select {
		case <-q.allReady:
		case <-time.After(10 * time.Second):
			t.Fatalf("started: got = %d, want = 100", q.started.Load())
		}
		// Wait returns once the request is blocked on the work alone, so the loop
		// has ended.
		synctest.Wait()
		if h.(*handler).repassing.Load() {
			t.Fatal("repassing: still set after the passes ended, work still running")
		}
		close(q.release)
		<-done
	})
}

// TestHandler_SlowEmptyPassDoesNotBlockTheRepassLoop asserts a request that is
// still enumerating, and then finds nothing, does not hold the loop against a
// later request that finds a batch-capped backlog.
func TestHandler_SlowEmptyPassDoesNotBlockTheRepassLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const slots = 100
		q := newFillQueue(250, slots)
		q.holdFirst = make(chan struct{})
		h := Handler(q, slots, 10, func(context.Context, string, workqueue.Options) error {
			<-q.release
			return nil
		}, 0, WithDispatchPeriod(time.Millisecond))
		releaseOnExit(t, q.release)
		releaseOnExit(t, q.holdFirst)

		first := serve(t, h)
		synctest.Wait()
		if q.passCount() < 1 {
			t.Fatal("first pass never began enumerating")
		}
		second := serve(t, h)
		select {
		case <-q.allReady:
		case <-time.After(10 * time.Second):
			t.Fatalf("started: got = %d, want = %d from the second request", q.started.Load(), slots)
		}
		close(q.holdFirst)
		close(q.release)
		<-first
		<-second
	})
}

// TestHandler_SlowFailingPassDoesNotBlockTheRepassLoop asserts a request whose
// claims are still in flight, and then all fail, does not hold the loop against
// a later request whose claims succeed.
func TestHandler_SlowFailingPassDoesNotBlockTheRepassLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const slots = 100
		q := newFillQueue(250, slots)
		gate := make(chan struct{})
		for i := range 20 {
			q.firstPass = append(q.firstPass, &fillKey{mockKey: &mockKey{name: fmt.Sprintf("bad-%02d", i)}, q: q, gate: gate})
		}
		h := Handler(q, slots, 10, func(context.Context, string, workqueue.Options) error {
			<-q.release
			return nil
		}, 0, WithDispatchPeriod(time.Millisecond))
		releaseOnExit(t, q.release)
		releaseOnExit(t, gate)

		first := serve(t, h)
		synctest.Wait()
		if q.passCount() < 1 {
			t.Fatal("first pass never began enumerating")
		}
		second := serve(t, h)
		select {
		case <-q.allReady:
		case <-time.After(10 * time.Second):
			t.Fatalf("started: got = %d, want = %d from the second request", q.started.Load(), slots)
		}
		close(gate)
		close(q.release)
		<-first
		<-second
	})
}
