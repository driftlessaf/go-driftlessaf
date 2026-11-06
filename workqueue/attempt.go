/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import "context"

// Attempt describes the dispatch attempt a callback runs under, so a callback
// can act on its last chance before the key is dead-lettered.
type Attempt struct {
	// Number counts the attempts on the key, this one included.
	Number int

	// Max is the attempt count at which a failure dead-letters the key. Zero
	// means retries are unbounded or the dispatcher did not report it.
	Max int
}

// Final reports whether a retriable failure of this attempt dead-letters the
// key rather than retrying it.
func (a Attempt) Final() bool {
	return a.Max > 0 && a.Number >= a.Max
}

type attemptKey struct{}

// WithAttempt returns ctx carrying a. The dispatcher sets it on the callback's
// context, and receivers set it from the ProcessRequest fields.
func WithAttempt(ctx context.Context, a Attempt) context.Context {
	return context.WithValue(ctx, attemptKey{}, a)
}

// AttemptFromContext returns the attempt ctx carries, and false when it
// carries none.
func AttemptFromContext(ctx context.Context) (Attempt, bool) {
	a, ok := ctx.Value(attemptKey{}).(Attempt)
	return a, ok
}
