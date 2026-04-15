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
package dispatcher
