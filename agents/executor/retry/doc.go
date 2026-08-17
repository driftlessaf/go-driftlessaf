/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package retry provides exponential backoff retry logic for handling transient errors.

# Overview

This package implements retry mechanisms with exponential backoff and jitter,
designed specifically for handling rate limit and quota errors from external APIs.
It integrates with the workqueue package to enable graceful backoff when retries
are exhausted.

# Features

  - Exponential backoff with configurable base and maximum delays
  - Random jitter to prevent thundering herd problems
  - Context-aware cancellation during retry loops
  - Integration with workqueue for deferred retry after exhaustion
  - Generic retry function supporting any return type
  - Configurable retry predicates to distinguish retryable from permanent errors

# Usage

The primary entry point is RetryWithBackoff, which executes a function with
automatic retry on transient errors:

	cfg := retry.DefaultRetryConfig()
	result, err := retry.RetryWithBackoff(
		ctx,
		cfg,
		"fetch_data",
		isRetryable,
		func() (string, error) {
			return api.FetchData()
		},
	)

For workqueue integration, use RequeueIfRetryable to convert exhausted retries
into a delayed requeue:

	err := doWorkWithRetries(ctx)
	if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryable, "OpenAI"); requeueErr != nil {
		return requeueErr
	}
	return err

The requeue is marked as an infrastructure failure and keeps the provider error
in its chain, so workqueue.IsInfrastructureError reports it as one and callers
can still match the provider error with errors.Is and errors.As.

# Configuration

RetryConfig controls retry behavior:

  - MaxRetries: Maximum number of retry attempts (0 disables retries)
  - BaseBackoff: Initial backoff duration (doubled each attempt)
  - MaxBackoff: Maximum backoff duration (caps exponential growth)
  - MaxJitter: Maximum random jitter added to each backoff

Use DefaultRetryConfig() for sensible defaults tuned for quota-based rate limits,
or construct a custom RetryConfig for specific requirements.

# Integration Patterns

Retry with workqueue integration:

	func (w *Worker) Process(ctx context.Context, item string) error {
		result, err := retry.RetryWithBackoff(
			ctx,
			retry.DefaultRetryConfig(),
			"process_item",
			isQuotaError,
			func() (string, error) {
				return w.api.Process(item)
			},
		)
		if err != nil {
			if requeueErr := retry.RequeueIfRetryable(ctx, err, isQuotaError, "API"); requeueErr != nil {
				return requeueErr
			}
			return err
		}
		return w.store.Save(result)
	}

Custom retry predicate:

	func isRetryable(err error) bool {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiErr.Code == 429 || apiErr.Code >= 500
		}
		return false
	}
*/
package retry
