/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"time"
)

// ErrorAction describes the disposition of a key after a dispatch error.
type ErrorAction int

const (
	// ErrorRequeued indicates the key was returned to the queue for retry.
	ErrorRequeued ErrorAction = iota

	// ErrorDeadLettered indicates the key exhausted its retry budget and was
	// moved to the dead-letter queue.
	ErrorDeadLettered

	// ErrorDropped indicates the error was marked non-retriable and the key
	// was completed without further processing.
	ErrorDropped
)

func (a ErrorAction) String() string {
	switch a {
	case ErrorRequeued:
		return "requeued"
	case ErrorDeadLettered:
		return "dead-lettered"
	case ErrorDropped:
		return "dropped"
	default:
		return "unknown"
	}
}

// ErrorContext carries information about a dispatch error.
type ErrorContext struct {
	// Key is the workqueue key that failed.
	Key string

	// Err is the error returned by the callback.
	Err error

	// Attempts is the number of times the key has been attempted (including
	// the current attempt).
	Attempts int

	// Action is what the dispatcher did with the key after the error.
	Action ErrorAction

	// NonRetriableReason is set when Action is ErrorDropped, providing
	// the reason the error was marked non-retriable.
	NonRetriableReason string
}

// errorEmitter is an internal interface for emitting dispatch errors.
type errorEmitter interface {
	emit(ctx context.Context, ec ErrorContext)
	drain()
}

// nopErrorEmitter discards all errors silently.
type nopErrorEmitter struct{}

func (nopErrorEmitter) emit(context.Context, ErrorContext) {}
func (nopErrorEmitter) drain()                             {}

// Option configures optional dispatcher behavior.
type Option func(*config)

type config struct {
	errors         errorEmitter
	backoff        func(attempts int) time.Duration
	dispatchPeriod time.Duration
}

// WithDispatchPeriod sets the minimum interval between dispatch passes for
// a Handler: at most one pass is admitted per period, and triggers beyond
// that are acknowledged and dropped. The default is one second. It has no
// effect on Handle or HandleAsync, which initiate unconditionally.
func WithDispatchPeriod(d time.Duration) Option {
	return func(c *config) {
		c.dispatchPeriod = d
	}
}

// WithBackoff sets the failure-retry backoff for the dispatcher. On each
// callback failure that is requeued (not dead-lettered, not dropped as
// non-retriable), the dispatcher calls fn with the key's current attempt count
// and, when fn returns a positive duration, requeues the key with that
// not-before delay while preserving the attempt count (so the dead-letter
// cutoff stays reachable).
//
// When fn is nil or returns a non-positive duration, the dispatcher falls back
// to a bare requeue, identical to the behavior with this option unset. This
// makes the option entirely opt-in: an unconfigured dispatcher keeps the
// existing requeue behavior bit-for-bit. Use it to install decorrelated
// exponential jitter or any other attempt-driven backoff curve.
func WithBackoff(fn func(attempts int) time.Duration) Option {
	return func(c *config) { c.backoff = fn }
}

func applyOptions(opts []Option) config {
	cfg := config{errors: nopErrorEmitter{}}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}
