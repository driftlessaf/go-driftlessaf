/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dag

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
)

// Run executes the graph and returns a result for every step. Logical
// outcomes, cancellation included, are reported per step rather than as an
// error.
func (g *Graph) Run(ctx context.Context, opts ...Option) Result {
	o := options{maxConcurrency: defaultMaxConcurrency}
	for _, opt := range opts {
		opt(&o)
	}

	results := make([]StepResult, len(g.nodes))
	remaining := make([]int, len(g.nodes))
	var ready []int
	for i, n := range g.nodes {
		remaining[i] = len(n.deps)
		if remaining[i] == 0 {
			ready = append(ready, i)
		}
	}
	release := func(i int) {
		for _, d := range g.nodes[i].dependents {
			if remaining[d]--; remaining[d] == 0 {
				ready = append(ready, d)
			}
		}
	}

	// Buffered so a finished runner never blocks on a coordinator that is
	// itself waiting for a free slot.
	done := make(chan int, len(g.nodes))
	running := 0
	for {
		for running < o.maxConcurrency && len(ready) > 0 {
			i := ready[0]
			ready = ready[1:]
			if r, blocked := g.blocked(ctx, i, results); blocked {
				results[i] = r
				release(i)
				continue
			}
			in := Input{Reads: make(map[string][]byte, len(g.nodes[i].deps))}
			for _, e := range g.nodes[i].deps {
				if r := results[e.from]; r.Status == StatusSucceeded {
					in.Reads[g.nodes[e.from].step.Name] = r.Output
				}
			}
			running++
			go func() {
				returned := false
				// A panic or runtime.Goexit here can't reach the caller; fail the
				// step so siblings finish and keep their saves.
				defer func() {
					if p := recover(); p != nil {
						results[i] = StepResult{Status: StatusFailed, Err: &panicError{step: g.nodes[i].step.Name, value: p, stack: debug.Stack()}}
					} else if !returned {
						results[i] = StepResult{Status: StatusFailed, Err: fmt.Errorf("dag: step %q exited its goroutine without returning (runtime.Goexit)", g.nodes[i].step.Name)}
					}
					done <- i
				}()
				results[i] = execute(ctx, g.nodes[i].step, in, o.resumer)
				returned = true
			}()
		}
		if running == 0 {
			break
		}
		i := <-done
		running--
		release(i)
	}

	steps := make(map[string]StepResult, len(g.nodes))
	for i, n := range g.nodes {
		steps[n.step.Name] = results[i]
	}
	return Result{Steps: steps}
}

// blocked decides, from the run and finished dependencies alone, whether step
// i must not run. A failed required dependency outranks a deferred one:
// waiting on a broken branch would only put off the skip.
func (g *Graph) blocked(ctx context.Context, i int, results []StepResult) (StepResult, bool) {
	if ctx.Err() != nil {
		return StepResult{Status: StatusSkipped, Err: context.Cause(ctx)}, true
	}
	var wait error
	for _, e := range g.nodes[i].deps {
		name := g.nodes[e.from].step.Name
		switch st := results[e.from].Status; st {
		case StatusFailed, StatusSkipped:
			if !e.optional {
				return StepResult{Status: StatusSkipped, Err: fmt.Errorf("dag: dependency %q %s", name, st)}, true
			}
		case StatusDeferred, StatusWaiting:
			if wait == nil {
				wait = fmt.Errorf("dag: dependency %q %s: %w", name, st, ErrDeferred)
			}
		}
	}
	if wait != nil {
		return StepResult{Status: StatusWaiting, Err: wait}, true
	}
	return StepResult{}, false
}

func execute(ctx context.Context, s Step, in Input, resumer Resumer) StepResult {
	if s.Hash != nil {
		h, err := s.Hash(ctx, in)
		if err != nil {
			return StepResult{Status: StatusFailed, Err: fmt.Errorf("dag: hash inputs of %q: %w", s.Name, err)}
		}
		in.Hash = h
	}
	if resumer != nil && s.Hash != nil {
		out, ok, err := resumer.Lookup(ctx, s.Name, in.Hash)
		switch {
		case err != nil:
			return StepResult{Status: StatusFailed, Err: fmt.Errorf("dag: look up stored output of %q: %w", s.Name, err)}
		case ok:
			return StepResult{Status: StatusSucceeded, Output: out, Resumed: true}
		}
	}
	out, err := s.Run(ctx, in)
	switch {
	case errors.Is(err, ErrDeferred):
		return StepResult{Status: StatusDeferred, Err: err}
	case err != nil:
		return StepResult{Status: StatusFailed, Err: err}
	}
	if resumer != nil && s.Hash != nil {
		// The output is already paid for; a cancel that lands now must not drop it.
		if err := resumer.Save(context.WithoutCancel(ctx), s.Name, in.Hash, out); err != nil {
			return StepResult{Status: StatusFailed, Err: fmt.Errorf("dag: save output of %q: %w", s.Name, err)}
		}
	}
	return StepResult{Status: StatusSucceeded, Output: out}
}

// panicError reports a panic recovered from a step's runner, Hash or Resumer.
type panicError struct {
	step  string
	value any
	stack []byte
}

func (e *panicError) Error() string {
	return fmt.Sprintf("dag: step %q panicked: %v\n%s", e.step, e.value, e.stack)
}
