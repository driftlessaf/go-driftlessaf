/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package testevals

import (
	"fmt"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
)

// observer wraps a *testing.T to implement evals.Observer
type observer struct {
	tb     *testing.T
	prefix string
	count  int64
}

// New creates a new Observer from a *testing.T
func New(t *testing.T) evals.Observer {
	return &observer{tb: t}
}

// NewPrefix creates a new Observer from a *testing.T with a message prefix
func NewPrefix(t *testing.T, prefix string) evals.Observer {
	return &observer{tb: t, prefix: prefix}
}

// Fail marks the test as failed with the given message
func (o *observer) Fail(msg string) {
	if o.prefix != "" {
		o.tb.Errorf("%s: %s", o.prefix, msg)
	} else {
		o.tb.Error(msg)
	}
}

// Log logs a message
func (o *observer) Log(msg string) {
	if o.prefix != "" {
		o.tb.Logf("%s: %s", o.prefix, msg)
	} else {
		o.tb.Log(msg)
	}
}

// Grade assigns a rating (0.0-1.0) with reasoning to the trace result
func (o *observer) Grade(score float64, reasoning string) {
	msg := fmt.Sprintf("Grade: %.2f - %s", score, reasoning)
	if o.prefix != "" {
		o.tb.Logf("%s: %s", o.prefix, msg)
	} else {
		o.tb.Log(msg)
	}
}

// Increment increments the observation counter
func (o *observer) Increment() {
	atomic.AddInt64(&o.count, 1)
}

// Total returns the number of observed instances
func (o *observer) Total() int64 {
	return atomic.LoadInt64(&o.count)
}
