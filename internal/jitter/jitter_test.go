/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package jitter

import (
	"testing"
	"time"
)

func TestAdd(t *testing.T) {
	// Non-positive durations must be returned unchanged without panicking.
	// A negative duration is the ACID-446 regression: it arises from
	// time.Until(resetTime) when a rate-limit reset has already elapsed, and
	// previously panicked via rand.Int63n(negative).
	tests := []struct {
		name string
		d    time.Duration
		want time.Duration
	}{
		{name: "negative", d: -5 * time.Second, want: -5 * time.Second},
		{name: "zero", d: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Add(tt.d); got != tt.want {
				t.Errorf("Add(%v) = %v, want %v", tt.d, got, tt.want)
			}
		})
	}
}

func TestAddPositiveWithinBounds(t *testing.T) {
	// Positive durations get 0%–100% jitter, so the result is in [d, 2d).
	// Run many iterations to exercise the randomness.
	const d = time.Minute
	for range 1000 {
		got := Add(d)
		if got < d || got >= 2*d {
			t.Fatalf("Add(%v) = %v, want within [%v, %v)", d, got, d, 2*d)
		}
	}
}
