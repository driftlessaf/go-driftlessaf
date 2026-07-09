/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package breaker

import (
	"math/rand/v2"
	"sync"
	"time"
)

// Defaults for New.
const (
	// DefaultFailureThreshold is the number of consecutive failures that trips
	// a key's circuit open.
	DefaultFailureThreshold = 5
	// DefaultBaseDelay is the initial backoff, doubling per consecutive failure.
	DefaultBaseDelay = 30 * time.Second
	// DefaultMaxDelay caps the backoff; it matches workqueue.MaximumRequeueFloor.
	DefaultMaxDelay = time.Hour
)

// circuit tracks consecutive failures for a key and the earliest time its next
// attempt may proceed.
type circuit struct {
	failures int
	retryAt  time.Time
}

// Breaker is a keyed circuit breaker for transient failures against remote
// dependencies. Once a key reaches the failure threshold, Allow short-circuits
// with a backoff delay until a single half-open probe per backoff window
// succeeds. State is in-memory and per-process. Safe for concurrent use.
type Breaker struct {
	threshold int
	baseDelay time.Duration
	maxDelay  time.Duration

	// now is injectable for deterministic tests.
	now func() time.Time

	mu       sync.Mutex
	circuits map[string]circuit
}

// Option configures a Breaker.
type Option func(*Breaker)

// WithFailureThreshold sets the number of consecutive failures that trips a
// key's circuit open.
func WithFailureThreshold(n int) Option {
	return func(b *Breaker) { b.threshold = n }
}

// WithBaseDelay sets the initial backoff delay.
func WithBaseDelay(d time.Duration) Option {
	return func(b *Breaker) { b.baseDelay = d }
}

// WithMaxDelay caps the backoff delay.
func WithMaxDelay(d time.Duration) Option {
	return func(b *Breaker) { b.maxDelay = d }
}

// New returns a Breaker with the default tuning, adjusted by opts.
func New(opts ...Option) *Breaker {
	b := &Breaker{
		threshold: DefaultFailureThreshold,
		baseDelay: DefaultBaseDelay,
		maxDelay:  DefaultMaxDelay,
		now:       time.Now,
		circuits:  make(map[string]circuit),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Allow reports whether an attempt for key may proceed. When it returns false,
// the returned delay is the remaining cooldown.
func (b *Breaker) Allow(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	c := b.circuits[key]
	if c.failures < b.threshold {
		return true, 0
	}

	now := b.now()
	if now.Before(c.retryAt) {
		return false, c.retryAt.Sub(now)
	}

	// Grant a single half-open probe. Pushing retryAt forward keeps concurrent
	// callers short-circuiting until the probe resolves via RecordSuccess or
	// RecordFailure.
	c.retryAt = now.Add(b.backoff(c.failures))
	b.circuits[key] = c
	return true, 0
}

// RecordSuccess marks key healthy, closing its circuit.
func (b *Breaker) RecordSuccess(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.circuits, key)
}

// RecordFailure records a failure for key and returns the backoff to wait
// before retrying.
func (b *Breaker) RecordFailure(key string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	c := b.circuits[key]
	c.failures++
	delay := b.backoff(c.failures)
	c.retryAt = b.now().Add(delay)
	b.circuits[key] = c
	return delay
}

// backoff returns the jittered, capped exponential backoff for the given number
// of consecutive failures.
func (b *Breaker) backoff(failures int) time.Duration {
	d := b.baseDelay
	for i := 1; i < failures && d < b.maxDelay; i++ {
		d *= 2
	}
	return addJitter(min(d, b.maxDelay))
}

// addJitter adds 0% to +100% random jitter to avoid thundering herd.
//
//nolint:gosec // Using weak random for jitter is fine, not cryptographic
func addJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + rand.N(d)
}
