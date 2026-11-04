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
// The calls run in three tiers, each starting once the one before it has
// finished:
//
//  1. every call matching neither predicate, in the pool;
//  2. calls matching afterSiblings (tools that declare
//     toolcall.Definition.AfterSiblings), in a second pool of the same bound:
//     each reads state the first tier writes, so it must see that state
//     finished;
//  3. calls matching heldOut (submit, suspend), sequentially in slot order: a
//     submission claims the turn's work is complete, and its result validators
//     may read state the other handlers produce (worktrees, files), so they
//     must observe the finished state rather than race the handlers still
//     producing it, the second tier's included.
//
// Slot order is preserved, so the callers' in-order result consumption is
// unaffected.
func DispatchToolCalls[Call any](calls []Call, concurrency int, afterSiblings, heldOut func(Call) bool, run func(i int, call Call)) {
	pool := func(pick func(Call) bool) {
		g := new(errgroup.Group)
		g.SetLimit(max(1, concurrency))
		for i, call := range calls {
			if !pick(call) {
				continue
			}
			g.Go(func() error {
				run(i, call)
				return nil
			})
		}
		_ = g.Wait()
	}
	pool(func(call Call) bool { return !heldOut(call) && !afterSiblings(call) })
	pool(func(call Call) bool { return !heldOut(call) && afterSiblings(call) })
	for i, call := range calls {
		if heldOut(call) {
			run(i, call)
		}
	}
}
