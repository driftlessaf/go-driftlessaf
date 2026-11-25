/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dag

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrDeferred is wrapped by a runner's error to report that the step's answer
// will arrive later and a later run should pick the step up.
var ErrDeferred = errors.New("dag: step deferred")

// Input is what a step receives once its dependencies have finished.
type Input struct {
	// Reads holds the output of each dependency that succeeded, by step name.
	// A runner must not modify it.
	Reads map[string][]byte
	// Hash is the step's input hash from Step.Hash, so a runner can record
	// it beside its output; empty when Step.Hash is nil.
	Hash string
}

// StepRunner does the work of one step.
type StepRunner func(ctx context.Context, in Input) ([]byte, error)

// Step is one named node of a graph.
type Step struct {
	// Name identifies the step and keys its output in a dependent's Reads.
	Name string
	// Deps are steps that must succeed before this one runs.
	Deps []string
	// OptionalDeps are steps this one waits for but runs without if they fail.
	OptionalDeps []string
	// Run does the step's work.
	Run StepRunner
	// Hash returns the step's input hash, which must cover everything its
	// output depends on. A nil Hash means the step is never resumed.
	Hash func(ctx context.Context, in Input) (string, error)
}

// Status is how a step ended in one run.
type Status int

const (
	// StatusSucceeded means the step ran, or was resumed, and produced an output.
	StatusSucceeded Status = iota + 1
	// StatusFailed means the runner, Hash or the Resumer returned an error,
	// panicked or exited its goroutine.
	StatusFailed
	// StatusSkipped means a required dependency did not succeed, or the run was
	// cancelled before the step started.
	StatusSkipped
	// StatusDeferred means the runner returned an error wrapping ErrDeferred.
	StatusDeferred
	// StatusWaiting means a dependency, over either kind of edge, was deferred
	// or waiting, so the step did not run in this pass.
	StatusWaiting
)

// String returns the lowercase name of s.
func (s Status) String() string {
	switch s {
	case StatusSucceeded:
		return "succeeded"
	case StatusFailed:
		return "failed"
	case StatusSkipped:
		return "skipped"
	case StatusDeferred:
		return "deferred"
	case StatusWaiting:
		return "waiting"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// StepResult is the outcome of one step in a run.
type StepResult struct {
	// Status is how the step ended.
	Status Status
	// Output is the step's output when Status is StatusSucceeded.
	Output []byte
	// Err says why the step did not succeed. A skipped step's Err is the
	// run's cancel cause or names the dependency; a waiting step's wraps
	// ErrDeferred.
	Err error
	// Resumed reports that Output came from the Resumer rather than the runner.
	Resumed bool
}

// Result is the outcome of a run.
type Result struct {
	// Steps holds one result per step, keyed by step name.
	Steps map[string]StepResult
}

// Resumer stores step outputs so a later run can skip a step an earlier run
// finished. Implementations must be safe for concurrent use.
type Resumer interface {
	// Lookup returns the output stored for step under hash, and false when
	// there is none. An error fails the step.
	Lookup(ctx context.Context, step, hash string) ([]byte, bool, error)
	// Save stores a step's fresh output under hash. An error fails the step.
	// Its ctx carries the run's values but no deadline or cancellation, so a
	// finished step's output is kept after the run is cancelled; Save must
	// bound its own wait.
	Save(ctx context.Context, step, hash string, output []byte) error
}

// Option configures a run.
type Option func(*options)

type options struct {
	maxConcurrency int
	resumer        Resumer
}

const defaultMaxConcurrency = 4

// WithMaxConcurrency caps how many steps execute at once. A value below one
// means one. Without it, the cap is 4.
func WithMaxConcurrency(n int) Option {
	return func(o *options) { o.maxConcurrency = max(n, 1) }
}

// WithResumer looks up each hashed step's stored output before running it, and
// saves each fresh success.
func WithResumer(r Resumer) Option {
	return func(o *options) { o.resumer = r }
}

// edge is a resolved dependency: the index of the step depended on.
type edge struct {
	from     int
	optional bool
}

type node struct {
	step       Step
	deps       []edge
	dependents []int
}

// Graph is a validated set of steps. It is immutable and safe for concurrent
// use; each Run keeps its own state.
type Graph struct {
	nodes []node
}

// New validates steps and returns their graph. It rejects an empty or
// duplicate name, a nil runner, a repeated or unknown dependency, and a cycle
// over either kind of edge.
func New(steps []Step) (*Graph, error) {
	idx := make(map[string]int, len(steps))
	for i, s := range steps {
		switch _, dup := idx[s.Name]; {
		case s.Name == "":
			return nil, fmt.Errorf("dag: step %d has no name", i)
		case dup:
			return nil, fmt.Errorf("dag: duplicate step %q", s.Name)
		case s.Run == nil:
			return nil, fmt.Errorf("dag: step %q has no runner", s.Name)
		}
		idx[s.Name] = i
	}

	nodes := make([]node, len(steps))
	for i, s := range steps {
		s.Deps, s.OptionalDeps = slices.Clone(s.Deps), slices.Clone(s.OptionalDeps)
		nodes[i].step = s
		seen := make(map[string]struct{}, len(s.Deps)+len(s.OptionalDeps))
		for j, d := range slices.Concat(s.Deps, s.OptionalDeps) {
			if _, dup := seen[d]; dup {
				return nil, fmt.Errorf("dag: step %q lists dependency %q more than once", s.Name, d)
			}
			seen[d] = struct{}{}
			from, ok := idx[d]
			if !ok {
				return nil, fmt.Errorf("dag: step %q depends on unknown step %q", s.Name, d)
			}
			nodes[i].deps = append(nodes[i].deps, edge{from: from, optional: j >= len(s.Deps)})
			nodes[from].dependents = append(nodes[from].dependents, i)
		}
	}
	if err := detectCycle(nodes); err != nil {
		return nil, err
	}
	return &Graph{nodes: nodes}, nil
}

// detectCycle reports the first cycle found, as the path of step names around it.
func detectCycle(nodes []node) error {
	const (
		unvisited = iota
		visiting
		done
	)
	color := make([]int, len(nodes))
	var stack []string
	var visit func(i int) error
	visit = func(i int) error {
		color[i] = visiting
		stack = append(stack, nodes[i].step.Name)
		for _, e := range nodes[i].deps {
			switch color[e.from] {
			case visiting:
				start := slices.Index(stack, nodes[e.from].step.Name)
				return fmt.Errorf("dag: cycle: %s -> %s", strings.Join(stack[start:], " -> "), nodes[e.from].step.Name)
			case unvisited:
				if err := visit(e.from); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[i] = done
		return nil
	}
	for i := range nodes {
		if color[i] == unvisited {
			if err := visit(i); err != nil {
				return err
			}
		}
	}
	return nil
}
