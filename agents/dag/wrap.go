/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dag

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/retry"
)

// ErrMaxIterations is wrapped by the error an [Iterate] runner returns when
// its step is still not done after the last iteration.
var ErrMaxIterations = errors.New("dag: iterations exhausted")

// Retry returns a runner that re-runs run with backoff, per cfg, while it
// fails with an error isRetryable accepts; a nil isRetryable retries nothing.
// It never retries an error wrapping [ErrDeferred], and once ctx is done it
// starts no attempt and returns [context.Cause]. operation names the step in
// retry logs. run is required: Retry panics if it is nil.
func Retry(cfg retry.RetryConfig, operation string, isRetryable func(error) bool, run StepRunner) StepRunner {
	if run == nil {
		panic("dag: Retry needs a non-nil run")
	}
	if isRetryable == nil {
		isRetryable = func(error) bool { return false }
	}
	return func(ctx context.Context, in Input) ([]byte, error) {
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("dag: retry config for %q: %w", operation, err)
		}
		out, err := retry.RetryWithBackoff(ctx, cfg, operation, func(err error) bool {
			return !errors.Is(err, ErrDeferred) && ctx.Err() == nil && isRetryable(err)
		}, func() ([]byte, error) {
			// The backoff select can pick a ready timer over a done ctx.
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			return run(ctx, in)
		})
		// A cancel during backoff surfaces as ctx.Err(); report its cause.
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return out, context.Cause(ctx)
		}
		return out, err
	}
}

// Iterate returns a runner that calls step until it reports done, at most
// maxIterations times; a value below one means one. step is required: Iterate
// panics if it is nil. prev is nil on the first pass and the previous pass's
// output after that, so a step can revise its last draft. An error from step
// ends the loop and is returned as is, and a cancelled ctx stops it before the
// next pass with [context.Cause].
//
// When the cap is reached, the runner returns the last output with an error
// wrapping [ErrMaxIterations]. Under [Graph.Run] that step ends
// [StatusFailed] and its output is dropped; to keep a best effort, wrap the
// runner and return the output with a nil error on ErrMaxIterations.
func Iterate(maxIterations int, step func(ctx context.Context, in Input, prev []byte) (out []byte, done bool, err error)) StepRunner {
	if step == nil {
		panic("dag: Iterate needs a non-nil step")
	}
	maxIterations = max(maxIterations, 1)
	return func(ctx context.Context, in Input) ([]byte, error) {
		var prev []byte
		for range maxIterations {
			// A cancelled run should not start another full iteration.
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			out, done, err := step(ctx, in, prev)
			switch {
			case err != nil:
				return nil, err
			case done:
				return out, nil
			}
			prev = out
		}
		return prev, fmt.Errorf("dag: not done after %d iterations: %w", maxIterations, ErrMaxIterations)
	}
}
