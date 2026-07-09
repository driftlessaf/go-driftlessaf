/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package transient

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Retry knobs; vars so tests can shrink them. The base delay outlasts the
// ~1s window during which Rekor v2 fails whole batches of pending adds.
var (
	retryAttempts  = 3
	retryBaseDelay = time.Second
	retryMaxJitter = 2 * time.Second
)

// transientError marks a failure as transient across error wrapping.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// Mark marks err as a transient failure so that Is reports true for it.
func Mark(err error) error {
	if err == nil {
		return nil
	}
	return &transientError{err: err}
}

// Is reports whether err is a transient failure: one marked by Retry or
// Mark, or a registry error with a temporary status code.
func Is(err error) bool {
	var te *transientError
	if errors.As(err, &te) {
		return true
	}
	var terr *transport.Error
	return errors.As(err, &terr) && terr.Temporary()
}

// Retry invokes fn, retrying failures that retryable reports worth retrying,
// with short jittered delays between attempts. Once attempts are exhausted
// or ctx is done, the last error is returned marked transient (see Is);
// non-retryable failures are returned as-is. op names the operation in retry
// logs.
func Retry(ctx context.Context, op string, retryable func(error) bool, fn func(context.Context) error) error {
	var err error
	for attempt := range retryAttempts {
		if attempt > 0 {
			delay := retryBaseDelay + rand.N(retryMaxJitter) //nolint:gosec // G404: jitter, not security-sensitive
			clog.WarnContextf(ctx, "Transient failure %s (attempt %d/%d), retrying in %v: %v", op, attempt, retryAttempts, delay, err)
			select {
			case <-ctx.Done():
				return Mark(err)
			case <-time.After(delay):
			}
		}
		if err = fn(ctx); err == nil || !retryable(err) {
			return err
		}
	}
	return Mark(err)
}
