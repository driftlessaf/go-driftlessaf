/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schedtest

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/dag"
)

// Config is the run configuration a [RunFunc] receives.
type Config struct {
	// MaxConcurrency caps concurrent runners; zero means the default.
	MaxConcurrency int
	// Resumer, when set, serves and stores step outputs.
	Resumer dag.Resumer
}

// Serial runs steps one at a time: each pass takes the first unfinished step,
// in declaration order, whose dependencies have all finished. It validates
// with dag.New and reimplements everything else.
func Serial(ctx context.Context, steps []dag.Step, cfg Config) (dag.Result, error) {
	if _, err := dag.New(steps); err != nil {
		return dag.Result{}, err
	}
	results := make(map[string]dag.StepResult, len(steps))
	for len(results) < len(steps) {
		for _, s := range steps {
			if _, finished := results[s.Name]; finished || !depsFinished(s, results) {
				continue
			}
			// Its own goroutine, as in dag.Run, so a runtime.Goexit fails the
			// step instead of ending the caller.
			ch := make(chan dag.StepResult, 1)
			go func() {
				var res dag.StepResult
				returned := false
				defer func() {
					if p := recover(); p != nil {
						res = dag.StepResult{Status: dag.StatusFailed, Err: fmt.Errorf("step %q panicked: %v", s.Name, p)}
					} else if !returned {
						res = dag.StepResult{Status: dag.StatusFailed, Err: fmt.Errorf("step %q exited its goroutine without returning", s.Name)}
					}
					ch <- res
				}()
				res = serialStep(ctx, s, results, cfg.Resumer)
				returned = true
			}()
			results[s.Name] = <-ch
			break
		}
	}
	return dag.Result{Steps: results}, nil
}

func depsFinished(s dag.Step, results map[string]dag.StepResult) bool {
	for _, deps := range [][]string{s.Deps, s.OptionalDeps} {
		for _, d := range deps {
			if _, ok := results[d]; !ok {
				return false
			}
		}
	}
	return true
}

func serialStep(ctx context.Context, s dag.Step, results map[string]dag.StepResult, resumer dag.Resumer) dag.StepResult {
	if ctx.Err() != nil {
		return dag.StepResult{Status: dag.StatusSkipped, Err: context.Cause(ctx)}
	}
	for _, d := range s.Deps {
		if st := results[d].Status; st == dag.StatusFailed || st == dag.StatusSkipped {
			return dag.StepResult{Status: dag.StatusSkipped, Err: fmt.Errorf("dependency %q %s", d, st)}
		}
	}
	in := dag.Input{Reads: make(map[string][]byte)}
	for _, deps := range [][]string{s.Deps, s.OptionalDeps} {
		for _, d := range deps {
			switch r := results[d]; r.Status {
			case dag.StatusDeferred, dag.StatusWaiting:
				return dag.StepResult{Status: dag.StatusWaiting, Err: fmt.Errorf("dependency %q %s: %w", d, r.Status, dag.ErrDeferred)}
			case dag.StatusSucceeded:
				in.Reads[d] = r.Output
			}
		}
	}

	if s.Hash != nil {
		h, err := s.Hash(ctx, in)
		if err != nil {
			return dag.StepResult{Status: dag.StatusFailed, Err: err}
		}
		in.Hash = h
		if resumer != nil {
			out, ok, err := resumer.Lookup(ctx, s.Name, h)
			if err != nil {
				return dag.StepResult{Status: dag.StatusFailed, Err: err}
			}
			if ok {
				return dag.StepResult{Status: dag.StatusSucceeded, Output: out, Resumed: true}
			}
		}
	}
	out, err := s.Run(ctx, in)
	if errors.Is(err, dag.ErrDeferred) {
		return dag.StepResult{Status: dag.StatusDeferred, Err: err}
	}
	if err != nil {
		return dag.StepResult{Status: dag.StatusFailed, Err: err}
	}
	if s.Hash != nil && resumer != nil {
		if err := resumer.Save(context.WithoutCancel(ctx), s.Name, in.Hash, out); err != nil {
			return dag.StepResult{Status: dag.StatusFailed, Err: err}
		}
	}
	return dag.StepResult{Status: dag.StatusSucceeded, Output: out}
}
