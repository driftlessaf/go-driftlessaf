/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/chainguard-dev/clog"
)

// RetryConfig configures retry behavior for API calls.
// This is particularly useful for handling rate limit and transient server errors.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts (default: 5)
	// 0 means do not retry at all.
	MaxRetries int
	// BaseBackoff is the initial backoff duration (default: 1s, higher than typical due to quota nature)
	BaseBackoff time.Duration
	// MaxBackoff is the maximum backoff duration (default: 60s)
	MaxBackoff time.Duration
	// MaxJitter is the maximum random jitter added to backoff (default: 500ms)
	MaxJitter time.Duration
}

// Validate checks that the retry configuration has valid values.
func (c RetryConfig) Validate() error {
	if c.MaxRetries < 0 {
		return errors.New("max retries cannot be negative")
	}
	if c.BaseBackoff < 0 {
		return errors.New("base backoff cannot be negative")
	}
	if c.MaxBackoff < 0 {
		return errors.New("max backoff cannot be negative")
	}
	if c.MaxJitter < 0 {
		return errors.New("max jitter cannot be negative")
	}
	return nil
}

// DefaultRetryConfig returns a retry configuration suitable for quota and rate limit errors.
// Uses longer backoffs than typical retry configs because quota-based rate limits
// often require more time to recover.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:  5,
		BaseBackoff: 1 * time.Second,
		MaxBackoff:  60 * time.Second,
		MaxJitter:   500 * time.Millisecond,
	}
}

// RetryWithBackoff executes the given function with exponential backoff retry.
// It only retries on errors that are classified as retryable by the provided isRetryable function.
func RetryWithBackoff[T any](ctx context.Context, cfg RetryConfig, operation string, isRetryable func(error) bool, fn func() (T, error)) (T, error) {
	var result T
	var lastErr error

	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		result, lastErr = fn()
		if lastErr == nil {
			return result, nil
		}

		if !isRetryable(lastErr) {
			return result, lastErr
		}

		if attempt >= cfg.MaxRetries {
			break
		}

		// Calculate exponential backoff: BaseBackoff * 2^attempt, capped at MaxBackoff
		backoff := min(cfg.BaseBackoff<<attempt, cfg.MaxBackoff)

		// Add random jitter to avoid thundering herd
		var jitter time.Duration
		if cfg.MaxJitter > 0 {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(cfg.MaxJitter)))
			if err == nil {
				jitter = time.Duration(n.Int64())
			}
		}

		clog.FromContext(ctx).With("operation", operation).
			With("attempt", attempt+1).
			With("max_retries", cfg.MaxRetries).
			With("backoff", backoff+jitter).
			With("error", lastErr.Error()).
			Warn("Rate limit hit, retrying")

		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(backoff + jitter):
		}
	}

	return result, fmt.Errorf("%s failed after %d retries: %w", operation, cfg.MaxRetries, lastErr)
}
