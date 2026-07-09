/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package breaker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testKey = "packages.example.dev"

// newTestBreaker returns a breaker whose clock reads *now.
func newTestBreaker(now *time.Time, opts ...Option) *Breaker {
	b := New(opts...)
	b.now = func() time.Time { return *now }
	return b
}

func TestLifecycle(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(&now)

	// Failures below the threshold back off but stay allowed.
	for range DefaultFailureThreshold - 1 {
		require.Positive(t, b.RecordFailure(testKey))
		ok, _ := b.Allow(testKey)
		require.True(t, ok, "circuit should stay closed below the threshold")
	}

	// At the threshold the circuit opens, reporting the remaining cooldown.
	delay := b.RecordFailure(testKey)
	ok, remaining := b.Allow(testKey)
	require.False(t, ok, "circuit should open at the threshold")
	require.Equal(t, delay, remaining)

	// After the cooldown exactly one half-open probe is granted.
	now = now.Add(remaining + time.Nanosecond)
	ok, _ = b.Allow(testKey)
	require.True(t, ok, "probe should be granted after the cooldown")
	ok, _ = b.Allow(testKey)
	require.False(t, ok, "concurrent callers should not get a second probe")

	// A failed probe reopens the circuit with a fresh cooldown.
	require.Positive(t, b.RecordFailure(testKey))
	ok, _ = b.Allow(testKey)
	require.False(t, ok, "circuit should reopen after a failed probe")

	// A success closes the circuit.
	b.RecordSuccess(testKey)
	ok, remaining = b.Allow(testKey)
	require.True(t, ok, "circuit should close after a success")
	require.Zero(t, remaining)
}

func TestOptions(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(&now,
		WithFailureThreshold(1),
		WithBaseDelay(time.Second),
		WithMaxDelay(2*time.Second),
	)

	require.Positive(t, b.RecordFailure(testKey))
	ok, _ := b.Allow(testKey)
	require.False(t, ok, "circuit should open at the configured threshold")

	// Jitter adds 0% to +100%, so the capped backoff stays within [max, 2*max].
	delay := b.RecordFailure(testKey)
	require.GreaterOrEqual(t, delay, 2*time.Second)
	require.LessOrEqual(t, delay, 4*time.Second)
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		name     string
		failures int
		base     time.Duration
	}{
		{"first_failure", 1, DefaultBaseDelay},
		{"doubles", 3, 4 * DefaultBaseDelay},
		{"caps_at_max", 1000, DefaultMaxDelay},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Jitter adds 0% to +100% of the base delay.
			got := New().backoff(tc.failures)
			require.GreaterOrEqual(t, got, tc.base)
			require.LessOrEqual(t, got, 2*tc.base)
		})
	}
}
