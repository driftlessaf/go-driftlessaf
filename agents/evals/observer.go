/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"path/filepath"
	"sort"
	"sync"

	"chainguard.dev/driftlessaf/agents/agenttrace"
)

// Observer defines an interface for observing and controlling evaluation execution
type Observer interface {
	// Fail marks the evaluation as failed with the given message
	// Should be called at most once per Trace evaluation
	Fail(string)
	// Log logs a message
	// Can be called multiple times per Trace evaluation
	Log(string)
	// Grade assigns a rating (0.0-1.0) with reasoning to the trace result
	// Should be called at most once per Trace evaluation
	Grade(score float64, reasoning string)
	// Increment is called each time a trace is evaluated
	Increment()
	// Total returns the number of observed instances
	Total() int64
}

// ObservableTraceCallback is a function that receives an Observer interface and completed traces
type ObservableTraceCallback[T any] func(Observer, *agenttrace.Trace[T])

// Inject creates a TraceCallback by injecting an Observer implementation into an ObservableTraceCallback
func Inject[T any](obs Observer, callback ObservableTraceCallback[T]) agenttrace.TraceCallback[T] {
	return func(trace *agenttrace.Trace[T]) {
		obs.Increment()
		callback(obs, trace)
	}
}

// NamespacedObserver provides hierarchical namespacing for Observer instances
type NamespacedObserver[T Observer] struct {
	name     string                            // The name of this namespace node
	inner    T                                 // The Observer instance for this namespace
	factory  func(string) T                    // Factory function to create new T instances
	children map[string]*NamespacedObserver[T] // Child namespaces
	mu       sync.Mutex                        // Protects children map
}

// NewNamespacedObserver creates a new root NamespacedObserver with the given factory function
func NewNamespacedObserver[T Observer](factory func(string) T) *NamespacedObserver[T] {
	return &NamespacedObserver[T]{
		name:     "/",
		inner:    factory("/"),
		factory:  factory,
		children: make(map[string]*NamespacedObserver[T]),
	}
}

// Fail delegates to the inner Observer instance
func (n *NamespacedObserver[T]) Fail(msg string) {
	n.inner.Fail(msg)
}

// Log delegates to the inner Observer instance
func (n *NamespacedObserver[T]) Log(msg string) {
	n.inner.Log(msg)
}

// Grade delegates to the inner Observer instance
func (n *NamespacedObserver[T]) Grade(score float64, reasoning string) {
	n.inner.Grade(score, reasoning)
}

// Increment delegates to the inner Observer instance
func (n *NamespacedObserver[T]) Increment() {
	n.inner.Increment()
}

// Total delegates to the inner Observer instance
func (n *NamespacedObserver[T]) Total() int64 {
	return n.inner.Total()
}

// Child returns the child namespace with the given name, creating it if necessary
func (n *NamespacedObserver[T]) Child(name string) *NamespacedObserver[T] {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Check if child already exists
	if child, exists := n.children[name]; exists {
		return child
	}

	// Create new child with properly joined path
	childPath := filepath.Join(n.name, name)
	child := &NamespacedObserver[T]{
		name:     childPath,
		inner:    n.factory(childPath),
		factory:  n.factory,
		children: make(map[string]*NamespacedObserver[T]),
	}

	// Store and return the new child
	n.children[name] = child
	return child
}

// Walk traverses the observer tree in depth-first order, calling the visitor function
// on the current node first, then on all children in sorted order by name
func (n *NamespacedObserver[T]) Walk(visitor func(string, T)) {
	// Visit current node
	visitor(n.name, n.inner)

	// Get children names and sort them
	n.mu.Lock()
	childNames := make([]string, 0, len(n.children))
	for name := range n.children {
		childNames = append(childNames, name)
	}
	n.mu.Unlock()

	sort.Strings(childNames)

	// Visit each child in sorted order
	for _, name := range childNames {
		n.mu.Lock()
		child := n.children[name]
		n.mu.Unlock()

		// Recursively walk the child
		child.Walk(visitor)
	}
}
