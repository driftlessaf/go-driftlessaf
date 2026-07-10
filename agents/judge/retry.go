/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"time"

	"chainguard.dev/driftlessaf/agents/executor/retry"
)

// DefaultRetryConfig is the retry policy Retry uses: three total attempts (the
// first call plus two retries) spaced by exponential backoff with jitter.
//
// Every judge error is transient — an LLM transport or JSON-parse blip, or
// time-correlated capacity shedding (429 / RESOURCE_EXHAUSTED). Retrying
// immediately back-to-back mostly resamples the same failure window, effectively
// collapsing three retries into one; the backoff spreads them across it. The
// attempt count matches the prior fixed three-try behavior — this spaces the
// retries, it does not add more.
func DefaultRetryConfig() retry.RetryConfig {
	return retry.RetryConfig{
		MaxRetries:  2,
		BaseBackoff: 1 * time.Second,
		MaxBackoff:  30 * time.Second,
		MaxJitter:   500 * time.Millisecond,
	}
}

// Retry calls j.Judge with DefaultRetryConfig, retrying transient judge errors
// with exponential backoff. It returns the first successful judgement, or the last
// error (wrapped) if every attempt fails; a cancelled context returns promptly with
// ctx.Err(). The caller decides how to treat an exhausted-retries error (e.g. skip
// the metric, or hold) so that a judge outage does not masquerade as a real verdict.
func Retry(ctx context.Context, j Interface, req *Request) (*Judgement, error) {
	return RetryWithConfig(ctx, j, req, DefaultRetryConfig())
}

// RetryWithConfig is Retry with an explicit retry policy. Every judge error is
// treated as retryable (all judge failures are transient); the backoff, jitter, and
// context-cancellation handling come from executor/retry.RetryWithBackoff. Tests and
// tuning-sensitive callers use this to override DefaultRetryConfig.
func RetryWithConfig(ctx context.Context, j Interface, req *Request, cfg retry.RetryConfig) (*Judgement, error) {
	return retry.RetryWithBackoff(ctx, cfg, "judge:"+string(req.Mode),
		func(error) bool { return true },
		func() (*Judgement, error) { return j.Judge(ctx, req) })
}
