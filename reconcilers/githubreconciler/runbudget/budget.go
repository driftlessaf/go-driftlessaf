/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package runbudget

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// InterruptionBound bounds the publication phase when the request has already
// ended before the budget did. The platform may end a request early, for
// example while a new revision rolls out, and the process is then on borrowed
// time: publication still runs detached from the cancellation so accepted work
// is not discarded, but only for this long. The bound is measured from the
// first time publication finds the request ended and shared by every later
// publication context of the run, so a run that publishes in several steps
// gets one bound in total rather than one per step.
const InterruptionBound = time.Minute

// Budget bounds one reconcile. Total is the wall time the whole reconcile may
// use; Reserve is the tail of it kept for publication (commit, push, pull
// request create or update, comments) after the agent stops. The agent phase
// therefore ends at start + Total - Reserve and publication at start + Total.
// Callers size Total below the platform's request cap so the run ends on its
// own terms rather than the platform's.
type Budget struct {
	Total   time.Duration
	Reserve time.Duration
}

// Validate reports whether the budget is usable: a positive total and a
// positive reserve strictly inside it.
func (b Budget) Validate() error {
	if b.Total <= 0 {
		return fmt.Errorf("run budget total must be positive, got %v", b.Total)
	}
	if b.Reserve <= 0 {
		return fmt.Errorf("run budget reserve must be positive, got %v", b.Reserve)
	}
	if b.Reserve >= b.Total {
		return fmt.Errorf("run budget reserve %v must be less than total %v", b.Reserve, b.Total)
	}
	return nil
}

// FromSeconds builds a budget from configured second counts, the shape bots
// read from their environment. A zero total returns the zero budget, which
// leaves the reconcile bounded only by the platform; any other total must be
// positive and leave the reserve strictly inside it (see Validate).
func FromSeconds(totalSeconds, reserveSeconds int) (Budget, error) {
	if totalSeconds == 0 {
		return Budget{}, nil
	}
	b := Budget{
		Total:   time.Duration(totalSeconds) * time.Second,
		Reserve: time.Duration(reserveSeconds) * time.Second,
	}
	if err := b.Validate(); err != nil {
		return Budget{}, err
	}
	return b, nil
}

// IsZero reports whether no budget is configured.
func (b Budget) IsZero() bool { return b == Budget{} }

// agentDeadline is when the agent phase of a run that started at start must end.
func (b Budget) agentDeadline(start time.Time) time.Time {
	return start.Add(b.Total - b.Reserve)
}

// publicationDeadline is when the publication phase of a run that started at
// start must end.
func (b Budget) publicationDeadline(start time.Time) time.Time {
	return start.Add(b.Total)
}

// run is the budget and start time recorded on a context by Start, the done
// channel of the request that context belongs to, and the interruption
// deadline every publication context of the run shares once the request has
// ended. It rides the context by pointer so that publication contexts derived
// from different points of the context tree, such as the commit and push in
// clonemanager and the pull request update in changemanager, see the same
// interruption deadline.
type run struct {
	budget Budget
	start  time.Time

	// requestDone is the Done channel of the context passed to Start. Only
	// that context ending counts as an interruption: a context below it can
	// end for reasons of its own, such as an agent context reaching the agent
	// deadline, and must not cost publication its reserve.
	requestDone <-chan struct{}

	interruptOnce     sync.Once
	interruptDeadline time.Time
}

// requestEnded reports whether the request the run belongs to has ended.
func (r *run) requestEnded() bool {
	select {
	case <-r.requestDone:
		return true
	default:
		return false
	}
}

// interruptionDeadline returns the run's interruption deadline, fixing it at
// now plus InterruptionBound on the first call.
func (r *run) interruptionDeadline(now time.Time) time.Time {
	r.interruptOnce.Do(func() {
		r.interruptDeadline = now.Add(InterruptionBound)
	})
	return r.interruptDeadline
}

// runKey is the context key under which the run rides.
type runKey struct{}

// Start records the budget and the run's start time on ctx so that
// AgentContext and PublicationContext, called anywhere below, derive their
// deadlines from the same origin. ctx is the request's context: its ending is
// what PublicationContext treats as an interruption. A zero budget records nothing and returns
// ctx unchanged, so an unconfigured bot keeps its unbounded behavior.
func Start(ctx context.Context, b Budget, start time.Time) context.Context {
	if b.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, runKey{}, &run{budget: b, start: start, requestDone: ctx.Done()})
}

// Started returns the budget and start time recorded on ctx by Start, and
// whether one was recorded.
func Started(ctx context.Context) (Budget, time.Time, bool) {
	r, ok := ctx.Value(runKey{}).(*run)
	if !ok {
		return Budget{}, time.Time{}, false
	}
	return r.budget, r.start, true
}

// AgentContext derives the context the agent phase runs under: it inherits
// ctx's cancellation, so a request that ends still stops the agent, and it
// carries the agent deadline when a budget was recorded with Start. Without a
// budget it is a plain child of ctx. The caller must call the returned cancel
// once the agent phase is over.
func AgentContext(ctx context.Context) (context.Context, context.CancelFunc) {
	r, ok := ctx.Value(runKey{}).(*run)
	if !ok {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, r.budget.agentDeadline(r.start))
}

// PublicationContext derives the context the publication phase runs under
// once the agent has stopped. When a budget was recorded with Start the result
// is detached from ctx's cancellation, so a request that ends after the agent
// returned cannot discard an accepted result, and it is bounded by the
// publication deadline; when the request passed to Start has already ended
// the bound is instead the shorter of that deadline and the run's interruption
// deadline (see InterruptionBound), since the platform is ending the request
// and only a short tail is worth spending. ctx itself ending for another
// reason, such as an agent context reaching the agent deadline, leaves the
// publication deadline alone. Values on ctx (worktree, trace, logger) carry
// over. Without a budget the result is ctx itself with a no-op cancel, so
// unconfigured callers see no change. The caller must call the returned cancel
// once publication is over.
func PublicationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	r, ok := ctx.Value(runKey{}).(*run)
	if !ok {
		return ctx, func() {}
	}
	deadline := r.budget.publicationDeadline(r.start)
	if r.requestEnded() {
		if bound := r.interruptionDeadline(time.Now()); bound.Before(deadline) {
			deadline = bound
		}
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

// End classifies how an agent phase ended.
type End int

const (
	// Finished means the agent returned on its own, with a result or an error
	// of its own making, while its context was still live.
	Finished End = iota
	// DeadlineReached means the agent context reached the budget's agent
	// deadline while the request itself was still live.
	DeadlineReached
	// Interrupted means the request ended before the agent finished; the
	// platform ended it, for example during a rollout.
	Interrupted
)

// String renders the classification for logs.
func (e End) String() string {
	switch e {
	case Finished:
		return "finished"
	case DeadlineReached:
		return "deadline"
	case Interrupted:
		return "interrupted"
	default:
		return fmt.Sprintf("end(%d)", int(e))
	}
}

// Classify reports how the agent phase ended from the agent's result error,
// its context, and the request context it derived from. A nil error is always
// Finished: a result that arrived is accepted whatever the clocks say. An error
// while the agent context is still live is the agent's own and also Finished.
// Otherwise the agent context ended: Interrupted when the request ended too,
// since the agent inherits its cancellation, and DeadlineReached when only the
// agent deadline fired.
func Classify(parent, agent context.Context, err error) End {
	if err == nil || agent.Err() == nil {
		return Finished
	}
	if parent.Err() != nil {
		return Interrupted
	}
	return DeadlineReached
}
