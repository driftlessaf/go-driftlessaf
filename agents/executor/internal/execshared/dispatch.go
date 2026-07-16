/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import "golang.org/x/sync/errgroup"

// SubmitPredicate returns the routing predicate for a run's terminal submit
// tool: a call routes to submit when its name is not registered as a regular
// tool, matches the submit tool's name, and a submit handler is configured
// (submitConfigured). Each executor builds one instance per Execute and uses
// it for both executeToolCall's dispatch switch and DispatchToolCalls'
// held-out-of-pool partition, so the two sites cannot drift.
func SubmitPredicate[Meta any](tools map[string]Meta, submitToolName string, submitConfigured bool) func(name string) bool {
	return func(name string) bool {
		_, registered := tools[name]
		return !registered && name == submitToolName && submitConfigured
	}
}

// DispatchToolCalls runs a single turn's tool calls under a bounded errgroup
// pool. The model may emit several independent tool calls in one turn
// (parallel tool use); a concurrency of 1 (or less) runs them strictly in
// order, higher values run them concurrently. run(i, call) must record its
// outcome in per-index slots so concurrent handlers never race on shared
// state, and must be safe for concurrent use when concurrency exceeds 1.
//
// Calls matching isSubmit are held out of the pool and run sequentially only
// after every pooled handler has finished: a submission claims the turn's
// work is complete, and its result validators may read state the other
// handlers produce (worktrees, files), so they must observe the finished
// state rather than race the handlers still producing it. Slot order is
// preserved, so the callers' in-order result consumption is unaffected.
func DispatchToolCalls[Call any](calls []Call, concurrency int, isSubmit func(Call) bool, run func(i int, call Call)) {
	g := new(errgroup.Group)
	g.SetLimit(max(1, concurrency))
	for i, call := range calls {
		if isSubmit(call) {
			continue
		}
		g.Go(func() error {
			run(i, call)
			return nil
		})
	}
	_ = g.Wait()
	for i, call := range calls {
		if isSubmit(call) {
			run(i, call)
		}
	}
}
