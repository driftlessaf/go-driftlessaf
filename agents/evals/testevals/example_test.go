/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package testevals_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/evals/testevals"
)

// ExampleNew demonstrates creating a basic testing observer.
func ExampleNew() {
	// Create a basic observer from a *testing.T
	obs := testevals.New(&testing.T{})

	// Use the observer with evaluation callbacks
	callback := func(o evals.Observer, trace *agenttrace.Trace[string]) {
		o.Log("Processing trace")
		if trace.Error != nil {
			o.Fail("Trace had an error")
		}
	}

	// Inject the observer into the callback
	_ = evals.Inject[string](obs, callback)
}

// ExampleNewPrefix demonstrates creating a testing observer with a message prefix.
func ExampleNewPrefix() {
	// Create an observer with a prefix for namespaced logging
	obs := testevals.NewPrefix(&testing.T{}, "tool-validation")

	// Use with a namespaced observer factory
	namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return testevals.NewPrefix(&testing.T{}, name)
	})

	// Create child observers for different evaluation aspects
	toolObs := namespacedObs.Child("tool-calls")
	errorObs := namespacedObs.Child("errors")

	// Use the observers
	_ = obs
	_ = toolObs
	_ = errorObs
}
