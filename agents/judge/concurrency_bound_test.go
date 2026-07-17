//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/judge"
)

// peakJudge records the peak number of concurrent Judge calls so a test can
// assert the semaphore never lets more than judgeConcurrency run at once.
type peakJudge struct {
	inflight atomic.Int64
	peak     atomic.Int64
	delay    time.Duration
}

func (c *peakJudge) Judge(_ context.Context, _ *judge.Request) (*judge.Judgement, error) {
	n := c.inflight.Add(1)
	for {
		old := c.peak.Load()
		if n <= old || c.peak.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(c.delay)
	c.inflight.Add(-1)
	return &judge.Judgement{}, nil
}

// TestJudgeWithLimitBoundsConcurrency drives many more callers than slots through
// judgeWithLimit and asserts the peak concurrency never exceeds the cap, that no
// call is dropped, and that the semaphore fully drains (no slot leak, balanced
// acquire/release).
func TestJudgeWithLimitBoundsConcurrency(t *testing.T) {
	if len(judgeSem) != 0 {
		t.Fatalf("precondition: semaphore not empty at start: %d", len(judgeSem))
	}

	fake := &peakJudge{delay: 5 * time.Millisecond}
	ctx := t.Context()
	var wg sync.WaitGroup
	callers := judgeConcurrency * 8
	var done atomic.Int64
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := judgeWithLimit(ctx, fake, &judge.Request{}); err != nil {
				t.Errorf("judgeWithLimit returned error: %v", err)
				return
			}
			done.Add(1)
		}()
	}
	wg.Wait()

	if got := fake.peak.Load(); got > int64(judgeConcurrency) {
		t.Fatalf("peak concurrency %d exceeded cap %d", got, judgeConcurrency)
	}
	if got := done.Load(); got != int64(callers) {
		t.Fatalf("completed %d calls, want %d", got, callers)
	}
	if len(judgeSem) != 0 {
		t.Fatalf("semaphore leaked slots: %d still held", len(judgeSem))
	}
}

// TestJudgeWithLimitContextCancel exercises the ctx.Done branch deterministically:
// with every slot held, the only ready select case is cancellation, so the call
// must return ctx.Err() without invoking the judge and without leaking a slot.
func TestJudgeWithLimitContextCancel(t *testing.T) {
	if len(judgeSem) != 0 {
		t.Fatalf("precondition: semaphore not empty at start: %d", len(judgeSem))
	}

	// Fill the semaphore so the send case blocks.
	for range judgeConcurrency {
		judgeSem <- struct{}{}
	}
	t.Cleanup(func() {
		for range judgeConcurrency {
			<-judgeSem
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	fake := &peakJudge{}
	_, err := judgeWithLimit(ctx, fake, &judge.Request{})
	if err == nil {
		t.Fatal("expected context error when semaphore is full and ctx is canceled")
	}
	if got := fake.inflight.Load(); got != 0 {
		t.Fatalf("judge was invoked on the cancel path (inflight peak %d)", got)
	}
	if len(judgeSem) != judgeConcurrency {
		t.Fatalf("cancel path disturbed the semaphore: %d held, want %d", len(judgeSem), judgeConcurrency)
	}
}
