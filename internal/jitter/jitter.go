/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package jitter

import (
	"math/rand/v2"
	"time"
)

// Add adds 0% to +100% random jitter to avoid thundering herd.
// A non-positive duration is returned unchanged, so callers that pass the
// result to workqueue.RequeueAfter stay safe when a rate-limit reset time has
// already elapsed and time.Until returns a negative duration.
//
//nolint:gosec // Using weak random for jitter is fine, not cryptographic
func Add(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + rand.N(d)
}
