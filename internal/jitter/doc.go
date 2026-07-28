/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package jitter adds randomized jitter to backoff and retry durations to
// avoid a thundering herd. It guards against non-positive inputs so callers can
// pass values like time.Until(resetTime) directly — a rate-limit reset that has
// already elapsed yields a negative duration, which must not panic.
package jitter
