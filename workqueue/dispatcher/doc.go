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
// By default a failed-and-requeued key reuses the workqueue's existing
// backoff. To install a custom failure-retry backoff (for example decorrelated
// exponential jitter), pass [WithBackoff]:
//
//	handler := dispatcher.Handler(wq, 10, 5, callback, 3,
//	    dispatcher.WithBackoff(func(attempts int) time.Duration {
//	        return backoff(attempts)
//	    }),
//	)
//
// The hook is called with the key's current attempt count on each requeued
// failure. A positive return value defers the retry by that duration while
// preserving the attempt count, so the dead-letter cutoff stays reachable. A
// nil hook or a non-positive return falls back to the default bare requeue,
// making the option entirely opt-in.
//
// # Infrastructure Failures
//
// A callback failure classified as infrastructure (see
// [workqueue.IsInfrastructureError]) bypasses the WithBackoff hook and is
// requeued on a dedicated curve: [workqueue.InfraBackoffPeriod] doubling per
// attempt up to [workqueue.MaximumInfraBackoffPeriod], plus jitter. These
// failures say nothing about the key — the receiving instance was killed
// mid-dispatch, or a dependency was down — so retries are spaced widely to
// keep a transient outage from burning through the dead-letter budget. The
// attempt count is still preserved, so a key that only ever fails this way
// (e.g. an input that OOM-kills its receiver) is still dead-lettered
// eventually.
package dispatcher
