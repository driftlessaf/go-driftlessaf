/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package dispatcher provides a workqueue dispatcher that dequeues keys and
// invokes a callback for each one.
//
// The dispatcher handles orphaned in-progress keys, concurrency limits, and
// batch sizing. Use Handle for synchronous dispatch or HandleAsync for
// non-blocking dispatch with a Future to await results.
//
// # Error Handling
//
// When a callback returns an error, the dispatcher requeues, dead-letters,
// or drops the key depending on the error type and retry budget. To emit
// these errors as CloudEvents, pass [WithErrorIngressURI]:
//
//	handler := dispatcher.Handler(wq, 10, 5, callback, 3,
//	    dispatcher.WithErrorIngressURI(ctx, ingressURI, "my-wq"),
//	)
//
// When the ingress URL is empty the option is a no-op, making the feature
// entirely opt-in.
//
// # Failure-Retry Backoff
//
// Every retriable callback failure requeues on a jittered doubling curve:
// [workqueue.BackoffPeriod] on the first attempt, doubling per attempt up to
// [workqueue.MaximumBackoffPeriod]. The fast first step keeps races and
// transient blips cheap; the widening keeps persistent failures — an
// infrastructure storm, a deterministic error awaiting a fix — from burning
// the dead-letter budget in minutes. The attempt count is always preserved,
// so the dead-letter cutoff stays reachable. Failure classification
// ([workqueue.IsInfrastructureError]) is surfaced on dispatch error events
// for observability but does not change scheduling.
//
// To replace the default curve (for example with decorrelated exponential
// jitter), pass [WithBackoff]:
//
//	handler := dispatcher.Handler(wq, 10, 5, callback, 3,
//	    dispatcher.WithBackoff(func(attempts int) time.Duration {
//	        return backoff(attempts)
//	    }),
//	)
//
// The hook is called with the key's current attempt count on each requeued
// failure; a positive return value replaces the default delay, and a nil
// hook or non-positive return keeps the default curve.
package dispatcher
