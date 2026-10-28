/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package dispatcher provides a workqueue dispatcher that dequeues keys and
// invokes a callback for each one.
//
// The dispatcher handles orphaned in-progress keys (requeued, or dead-lettered
// once their attempt count meets maxRetry), concurrency limits, and
// batch sizing. Use Handle for synchronous dispatch or HandleAsync for
// non-blocking dispatch with a Future to await results.
//
// # Candidate Selection
//
// A queue with several dispatchers (one per region, say) hands each of them
// the same ordering, so taking the head means they race for the same keys and
// every claim but one is lost. When the queue reports an identity, each pass
// shuffles the head of each priority run before launching, which makes
// concurrent dispatchers tend to pick different keys and spreads work evenly
// across them.
//
// Priority still decides: a lower-priority key is never launched while a
// higher-priority one is available. Within a priority the shuffle covers the
// oldest [DefaultCandidateWindowFactor] candidates per launchable key, or the
// whole run when it is shorter, so a key can be passed over by at most that
// many others in one pass, and a fresh shuffle each pass gives every candidate
// in the window the same chance every time.
//
// Keys that implement [workqueue.TenantKey] are shuffled per tenant round
// instead of per priority run, so that no key jumps ahead of another tenant's
// earlier round. A round holding few tenants is a short group and spreads
// less, so dispatchers contend more for it. Consecutive rounds holding only
// one tenant's key are shuffled together, which keeps the tail of a
// single-tenant flood spread across dispatchers.
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
// When the trigger request contains a valid W3C traceparent header, emitted
// error events include its trace and span IDs. Recorders can use those IDs to
// correlate an error with logs and traces.
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
// the dead-letter budget in minutes. A backoff requeue preserves the attempt
// count, so the dead-letter cutoff stays reachable while nothing new arrives.
// A [workqueue.Interface.Queue] that pulls the pending backoff earlier is new
// input and resets the count to zero, so a key that fails deterministically
// while its events keep arriving retries once per event instead of
// dead-lettering. Failure classification
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
