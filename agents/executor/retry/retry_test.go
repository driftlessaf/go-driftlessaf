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

func TestRetryWithBackoff_Success(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	result, err := retry.RetryWithBackoff(context.Background(), testRetryConfig(), "test_op", alwaysRetryable, func() (string, error) {
		attempts.Add(1)
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected result %q, got %q", "ok", result)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected 1 attempt, got %d", got)
	}
}

func TestRetryWithBackoff_SuccessAfterRetries(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	retryableErr := errors.New("429 RESOURCE_EXHAUSTED")

	result, err := retry.RetryWithBackoff(context.Background(), testRetryConfig(), "test_op", alwaysRetryable, func() (string, error) {
		n := attempts.Add(1)
		if n < 3 {
			return "", retryableErr
		}
		return "recovered", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "recovered" {
		t.Fatalf("expected result %q, got %q", "recovered", result)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestRetryWithBackoff_ExhaustedRetries(t *testing.T) {
	t.Parallel()
	cfg := testRetryConfig()
	cfg.MaxRetries = 3
	retryableErr := errors.New("Resource exhausted: quota exceeded")

	var attempts atomic.Int32
	_, err := retry.RetryWithBackoff(context.Background(), cfg, "test_op", alwaysRetryable, func() (string, error) {
		attempts.Add(1)
		return "", retryableErr
	})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}

	// Should have made MaxRetries+1 total attempts
	if got := attempts.Load(); got != 4 {
		t.Fatalf("expected 4 attempts (1 initial + 3 retries), got %d", got)
	}

	// Error should be wrapped with operation context
	if !errors.Is(err, retryableErr) {
		t.Fatalf("expected wrapped error to contain original, got: %v", err)
	}
	expected := fmt.Sprintf("test_op failed after %d retries", cfg.MaxRetries)
	if got := err.Error(); got[:len(expected)] != expected {
		t.Fatalf("expected error to start with %q, got %q", expected, got)
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
	_, err := retry.RetryWithBackoff(context.Background(), testRetryConfig(), "test_op", isRetryable, func() (string, error) {
		attempts.Add(1)
		return "", permErr
	})
	if err == nil {
		t.Fatal("expected error for non-retryable failure")
	}
	if !errors.Is(err, permErr) {
		t.Fatalf("expected original error, got: %v", err)
	}
	// Should stop immediately without retrying
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected 1 attempt (no retries for non-retryable error), got %d", got)
	}
}

func TestRetryWithBackoff_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
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
		t.Fatal("expected error on context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestRetryWithBackoff_ZeroRetries(t *testing.T) {
	t.Parallel()
	cfg := testRetryConfig()
	cfg.MaxRetries = 0
	retryableErr := errors.New("429 RESOURCE_EXHAUSTED")

	var attempts atomic.Int32
	_, err := retry.RetryWithBackoff(context.Background(), cfg, "test_op", alwaysRetryable, func() (string, error) {
		attempts.Add(1)
		return "", retryableErr
	})
	if err == nil {
		t.Fatal("expected error with zero retries")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected 1 attempt (no retries), got %d", got)
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
