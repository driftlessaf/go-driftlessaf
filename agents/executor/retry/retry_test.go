/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package retry_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/workqueue"
)

func testRetryConfig() retry.RetryConfig {
	return retry.RetryConfig{
		MaxRetries:  3,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
		MaxJitter:   time.Millisecond,
	}
}

// alwaysRetryable is a test helper that considers all errors retryable.
func alwaysRetryable(err error) bool {
	return err != nil
}

// OnAttemptError must fire once for each retryable attempt that triggered a
// retry — but NOT for the final attempt's err (success or terminal). This is
// the contract that lets agenttrace.LLMTurn.RecordError record transient
// errors without double-counting against LLMTurn.Fail (which records the
// terminal cause separately).
func TestRetryWithBackoff_OnAttemptError(t *testing.T) {
	t.Parallel()

	t.Run("fires once per retryable attempt that triggers a retry", func(t *testing.T) {
		t.Parallel()
		retryableErr := errors.New("429")
		var captured []string
		cfg := testRetryConfig()
		cfg.OnAttemptError = func(err error) { captured = append(captured, err.Error()) }

		var attempts atomic.Int32
		_, err := retry.RetryWithBackoff(t.Context(), cfg, "op", alwaysRetryable, func() (string, error) {
			n := attempts.Add(1)
			if n < 3 {
				return "", retryableErr
			}
			return "ok", nil
		})
		if err != nil {
			t.Fatalf("err: got %v, want nil", err)
		}
		// Two retryable attempts triggered retries; the third succeeded.
		// OnAttemptError fires on the first two only — the third's "success"
		// has no error to surface.
		if got, want := len(captured), 2; got != want {
			t.Fatalf("OnAttemptError calls: got %d, want %d", got, want)
		}
	})

	t.Run("does not fire for non-retryable terminal error", func(t *testing.T) {
		t.Parallel()
		nonRetryable := errors.New("401")
		var captured []string
		cfg := testRetryConfig()
		cfg.OnAttemptError = func(err error) { captured = append(captured, err.Error()) }

		_, err := retry.RetryWithBackoff(t.Context(), cfg, "op", func(error) bool { return false }, func() (string, error) {
			return "", nonRetryable
		})
		if err == nil {
			t.Fatal("err: got nil, want non-nil")
		}
		// Non-retryable errors propagate to the caller's terminal handler
		// (Fail), so OnAttemptError must stay silent to avoid double-recording.
		if got := len(captured); got != 0 {
			t.Fatalf("OnAttemptError calls: got %d, want 0", got)
		}
	})

	t.Run("does not fire for the final retryable error after exhaustion", func(t *testing.T) {
		t.Parallel()
		retryableErr := errors.New("503")
		var captured []string
		cfg := testRetryConfig()
		cfg.MaxRetries = 2
		cfg.OnAttemptError = func(err error) { captured = append(captured, err.Error()) }

		_, err := retry.RetryWithBackoff(t.Context(), cfg, "op", alwaysRetryable, func() (string, error) {
			return "", retryableErr
		})
		if err == nil {
			t.Fatal("err: got nil, want non-nil")
		}
		// MaxRetries=2 means 3 attempts total. Attempts 0 and 1 trigger
		// retries (callback fires twice). Attempt 2 fails but exhausts —
		// its err comes back wrapped via the return value, NOT via the
		// callback. Caller's Fail records the wrapped err, completing
		// the picture without duplication.
		if got, want := len(captured), 2; got != want {
			t.Fatalf("OnAttemptError calls: got %d, want %d", got, want)
		}
	})
}

func TestRetryWithBackoff_Success(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	result, err := retry.RetryWithBackoff(t.Context(), testRetryConfig(), "test_op", alwaysRetryable, func() (string, error) {
		attempts.Add(1)
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("error: got = %v, want = nil", err)
	}
	if result != "ok" {
		t.Fatalf("result: got = %q, want = %q", result, "ok")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts: got = %d, want = 1", got)
	}
}

func TestRetryWithBackoff_SuccessAfterRetries(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	retryableErr := errors.New("429 RESOURCE_EXHAUSTED")

	result, err := retry.RetryWithBackoff(t.Context(), testRetryConfig(), "test_op", alwaysRetryable, func() (string, error) {
		n := attempts.Add(1)
		if n < 3 {
			return "", retryableErr
		}
		return "recovered", nil
	})
	if err != nil {
		t.Fatalf("error: got = %v, want = nil", err)
	}
	if result != "recovered" {
		t.Fatalf("result: got = %q, want = %q", result, "recovered")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts: got = %d, want = 3", got)
	}
}

func TestRetryWithBackoff_ExhaustedRetries(t *testing.T) {
	t.Parallel()
	cfg := testRetryConfig()
	cfg.MaxRetries = 3
	retryableErr := errors.New("Resource exhausted: quota exceeded")

	var attempts atomic.Int32
	_, err := retry.RetryWithBackoff(t.Context(), cfg, "test_op", alwaysRetryable, func() (string, error) {
		attempts.Add(1)
		return "", retryableErr
	})
	if err == nil {
		t.Fatal("error: got = nil, want = non-nil after exhausted retries")
	}

	// Should have made MaxRetries+1 total attempts
	if got := attempts.Load(); got != 4 {
		t.Fatalf("attempts: got = %d, want = 4 (1 initial + 3 retries)", got)
	}

	// Error should be wrapped with operation context
	if !errors.Is(err, retryableErr) {
		t.Fatalf("wrapped error: got = %v, want = error containing original", err)
	}
	expected := fmt.Sprintf("test_op failed after %d retries", cfg.MaxRetries)
	if got := err.Error(); got[:len(expected)] != expected {
		t.Fatalf("error prefix: got = %q, want = %q", got[:len(expected)], expected)
	}
}

func TestRetryWithBackoff_NonRetryableError(t *testing.T) {
	t.Parallel()
	permErr := errors.New("permission denied: insufficient access")

	// Use an isRetryable that rejects this specific error
	isRetryable := func(err error) bool {
		return false
	}

	var attempts atomic.Int32
	_, err := retry.RetryWithBackoff(t.Context(), testRetryConfig(), "test_op", isRetryable, func() (string, error) {
		attempts.Add(1)
		return "", permErr
	})
	if err == nil {
		t.Fatal("error: got = nil, want = non-nil for non-retryable failure")
	}
	if !errors.Is(err, permErr) {
		t.Fatalf("error: got = %v, want = original error", err)
	}
	// Should stop immediately without retrying
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts: got = %d, want = 1 (no retries for non-retryable error)", got)
	}
}

func TestRetryWithBackoff_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	retryableErr := errors.New("429 rate limit exceeded")

	var attempts atomic.Int32
	// Cancel context after first attempt to interrupt backoff sleep
	_, err := retry.RetryWithBackoff(ctx, testRetryConfig(), "test_op", alwaysRetryable, func() (string, error) {
		n := attempts.Add(1)
		if n == 1 {
			// Cancel after first failure, before backoff sleep completes
			cancel()
		}
		return "", retryableErr
	})
	if err == nil {
		t.Fatal("error: got = nil, want = non-nil on context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got = %v, want = context.Canceled", err)
	}
}

func TestRetryWithBackoff_ZeroRetries(t *testing.T) {
	t.Parallel()
	cfg := testRetryConfig()
	cfg.MaxRetries = 0
	retryableErr := errors.New("429 RESOURCE_EXHAUSTED")

	var attempts atomic.Int32
	_, err := retry.RetryWithBackoff(t.Context(), cfg, "test_op", alwaysRetryable, func() (string, error) {
		attempts.Add(1)
		return "", retryableErr
	})
	if err == nil {
		t.Fatal("error: got = nil, want = non-nil with zero retries")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts: got = %d, want = 1 (no retries)", got)
	}
}

func TestRequeueIfRetryable_RetryableError(t *testing.T) {
	t.Parallel()
	// providerErr stands in for the provider's rate limit error, wrapped the way
	// RetryWithBackoff wraps it once the inner retries are exhausted.
	providerErr := errors.New("429 rate limit")
	exhaustedErr := fmt.Errorf("generate failed after 3 retries: %w", providerErr)
	isRetryable := func(err error) bool { return errors.Is(err, providerErr) }

	got := retry.RequeueIfRetryable(t.Context(), exhaustedErr, isRetryable, "TestProvider")
	if got == nil {
		t.Fatal("RequeueIfRetryable() = nil, want RequeueAfter error")
	}
	delay, floor, ok := workqueue.GetRequeueOptions(got)
	if !ok {
		t.Fatal("GetRequeueOptions() ok = false, want true")
	}
	if delay != retry.LLMBackoffDelay {
		t.Errorf("RequeueAfter delay = %v, want %v", delay, retry.LLMBackoffDelay)
	}
	// RequeueAfter, not RequeueNotBefore: a fresh event may pull the key forward.
	if floor {
		t.Errorf("RequeueAfter floor = true, want false")
	}
	// The dispatcher classifies the requeue as infrastructure churn rather than
	// an application verdict, which keeps the failure-rate dashboards readable.
	if !workqueue.IsInfrastructureError(got) {
		t.Errorf("IsInfrastructureError() = false, want true")
	}
	// The marker must not hide the provider cause: callers still match it.
	if !errors.Is(got, providerErr) {
		t.Errorf("errors.Is(got, providerErr) = false, want true")
	}
}

func TestRequeueIfRetryable_NonRetryableError(t *testing.T) {
	t.Parallel()
	permErr := errors.New("permission denied")
	isRetryable := func(error) bool { return false }

	got := retry.RequeueIfRetryable(t.Context(), permErr, isRetryable, "TestProvider")
	if got != nil {
		t.Errorf("RequeueIfRetryable() = %v, want nil", got)
	}
	// A non-retryable error is an application verdict, so nothing it returns may
	// carry the infrastructure marker.
	if workqueue.IsInfrastructureError(got) {
		t.Errorf("IsInfrastructureError() = true, want false")
	}
}

func TestDefaultRetryConfig(t *testing.T) {
	t.Parallel()
	cfg := retry.DefaultRetryConfig()

	if cfg.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", cfg.MaxRetries)
	}
	if cfg.BaseBackoff != time.Second {
		t.Errorf("BaseBackoff = %v, want %v", cfg.BaseBackoff, time.Second)
	}
	if cfg.MaxBackoff != 60*time.Second {
		t.Errorf("MaxBackoff = %v, want %v", cfg.MaxBackoff, 60*time.Second)
	}
	if cfg.MaxJitter != 500*time.Millisecond {
		t.Errorf("MaxJitter = %v, want %v", cfg.MaxJitter, 500*time.Millisecond)
	}
}
