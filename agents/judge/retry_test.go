/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/judge"
)

// countingJudge fails its first failFirst calls with err, then returns judgment.
type countingJudge struct {
	mu        sync.Mutex
	calls     int
	failFirst int
	err       error
	judgment  *judge.Judgement
}

func (c *countingJudge) Judge(context.Context, *judge.Request) (*judge.Judgement, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= c.failFirst {
		return nil, c.err
	}
	return c.judgment, nil
}

func (c *countingJudge) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fastRetry retries without real backoff so the tests do not sleep; the backoff
// timing itself is covered by the executor/retry package.
func fastRetry() retry.RetryConfig { return retry.RetryConfig{MaxRetries: 2} }

func TestRetry_SucceedsFirstCall(t *testing.T) {
	j := &countingJudge{judgment: &judge.Judgement{Score: 0.9}}
	// The default policy is used here; because the first call succeeds there is no
	// backoff sleep, so this exercises Retry without slowing the test.
	got, err := judge.Retry(t.Context(), j, &judge.Request{Mode: judge.GoldenMode})
	if err != nil {
		t.Fatalf("Retry() error = %v, want nil", err)
	}
	if got.Score != 0.9 {
		t.Errorf("Score: got = %v, want = 0.9", got.Score)
	}
	if j.callCount() != 1 {
		t.Errorf("judge calls: got = %d, want = 1", j.callCount())
	}
}

func TestRetryWithConfig_RecoversAfterTransientErrors(t *testing.T) {
	j := &countingJudge{
		failFirst: 2,
		err:       errors.New("no content generated - candidate content is nil"),
		judgment:  &judge.Judgement{Score: 0.8},
	}
	got, err := judge.RetryWithConfig(t.Context(), j, &judge.Request{Mode: judge.GoldenMode}, fastRetry())
	if err != nil {
		t.Fatalf("RetryWithConfig() error = %v, want nil (should recover on the third try)", err)
	}
	if got.Score != 0.8 {
		t.Errorf("Score: got = %v, want = 0.8", got.Score)
	}
	if j.callCount() != 3 {
		t.Errorf("judge calls: got = %d, want = 3 (first + two retries)", j.callCount())
	}
}

func TestRetryWithConfig_SurfacesLastErrorAfterExhaustion(t *testing.T) {
	sentinel := errors.New("persistent judge failure")
	j := &countingJudge{failFirst: 99, err: sentinel}
	_, err := judge.RetryWithConfig(t.Context(), j, &judge.Request{Mode: judge.GoldenMode}, fastRetry())
	if err == nil {
		t.Fatal("RetryWithConfig() error = nil, want the exhausted-retries error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the last judge error, got: %v", err)
	}
	if j.callCount() != 3 {
		t.Errorf("judge calls: got = %d, want = 3", j.callCount())
	}
}

func TestRetryWithConfig_ContextCancelled(t *testing.T) {
	j := &countingJudge{failFirst: 99, err: errors.New("transient")}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A long backoff (not fastRetry's zero) makes ctx.Done() the only ready case in
	// RetryWithBackoff's inter-attempt select — otherwise time.After(0) could win the
	// coin flip and the retries would exhaust with the transient error. The test still
	// returns instantly because the cancelled ctx short-circuits the first sleep.
	cfg := retry.RetryConfig{MaxRetries: 2, BaseBackoff: time.Hour, MaxBackoff: time.Hour}
	if _, err := judge.RetryWithConfig(ctx, j, &judge.Request{Mode: judge.GoldenMode}, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("RetryWithConfig() error = %v, want context.Canceled", err)
	}
}
