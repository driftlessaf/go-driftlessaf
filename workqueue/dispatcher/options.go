/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import "context"

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

// Option configures optional dispatcher behaviour.
type Option func(*config)

type config struct {
	errors errorEmitter
}

func applyOptions(opts []Option) config {
	cfg := config{errors: nopErrorEmitter{}}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}
