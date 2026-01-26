/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import "sync"

// Grade represents a grade with score and reasoning
type Grade struct {
	Score     float64
	Reasoning string
}

// ResultCollector wraps an Observer to collect failure messages and grades
type ResultCollector struct {
	inner    Observer
	failures []string
	grades   []Grade
	mu       sync.Mutex
}

// NewResultCollector creates a new ResultCollector that wraps the given Observer
func NewResultCollector(inner Observer) *ResultCollector {
	return &ResultCollector{
		inner:    inner,
		failures: make([]string, 0),
		grades:   make([]Grade, 0),
	}
}

// Fail logs the failure message and stores it in the failures list
func (r *ResultCollector) Fail(msg string) {
	// Log to inner observer (not Fail)
	r.inner.Log(msg)

	// Store the failure message
	r.mu.Lock()
	r.failures = append(r.failures, msg)
	r.mu.Unlock()
}

// Log passes through to the inner observer
func (r *ResultCollector) Log(msg string) {
	r.inner.Log(msg)
}

// Grade passes through to the inner observer and stores the grade
func (r *ResultCollector) Grade(score float64, reasoning string) {
	// Pass through to inner observer
	r.inner.Grade(score, reasoning)

	// Store the grade
	r.mu.Lock()
	r.grades = append(r.grades, Grade{
		Score:     score,
		Reasoning: reasoning,
	})
	r.mu.Unlock()
}

// Failures returns a copy of all collected failure messages
func (r *ResultCollector) Failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Return a copy to avoid external modifications
	result := make([]string, len(r.failures))
	copy(result, r.failures)
	return result
}

// Grades returns a copy of all collected grades
func (r *ResultCollector) Grades() []Grade {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Return a copy to avoid external modifications
	result := make([]Grade, len(r.grades))
	copy(result, r.grades)
	return result
}

// Increment passes through to the inner observer
func (r *ResultCollector) Increment() {
	r.inner.Increment()
}

// Total passes through to the inner observer
func (r *ResultCollector) Total() int64 {
	return r.inner.Total()
}
