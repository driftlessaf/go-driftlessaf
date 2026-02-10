/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package testevals provides a testing.T adapter for the evals framework.
//
// # Overview
//
// The testevals package provides adapters for *testing.T to implement the
// evals.Observer interface. This allows evaluation callbacks to report
// failures and log messages through Go's standard testing framework.
//
// Two constructors are available:
//   - New(t): Creates a basic adapter
//   - NewPrefix(t, prefix): Creates an adapter that prefixes all messages
//
// # Usage
//
// Basic usage with a test:
//
//	func TestLogAnalyzer(t *testing.T) {
//	    // Create a namespaced observer using testevals.NewPrefix
//	    namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
//	        return testevals.NewPrefix(t, name)
//	    })
//
//	    // Use evals helpers with the testing adapter
//	    callbacks := []agenttrace.TraceCallback{
//	        evals.Inject(namespacedObs.Child("tool-calls"), evals.ExactToolCalls(1)),
//	        evals.Inject(namespacedObs.Child("errors"), evals.NoErrors()),
//	    }
//
//	    tracer := agenttrace.ByCode(callbacks...)
//	    // Use tracer with your analyzer
//	}
//
// # Integration with Test Harnesses
//
// The observer adapter is particularly useful when building test harnesses
// that run multiple test cases with different evaluation criteria:
//
//	type TestCase struct {
//	    Name   string
//	    Evals  map[string]evals.ObservableTraceCallback
//	}
//
//	func runTestCase(t *testing.T, tc TestCase) {
//	    t.Run(tc.Name, func(t *testing.T) {
//	        // Create a namespaced observer using testevals.NewPrefix
//	        namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
//	            return testevals.NewPrefix(t, name)
//	        })
//
//	        var callbacks []agenttrace.TraceCallback
//	        for namespace, eval := range tc.Evals {
//	            childObs := namespacedObs.Child(namespace)
//	            callbacks = append(callbacks, evals.Inject(childObs, eval))
//	        }
//
//	        tracer := agenttrace.ByCode(callbacks...)
//	        // Run analyzer with tracer
//	    })
//	}
//
// # Thread Safety
//
// The observer adapter is thread-safe because it delegates to *testing.T,
// which is designed to be called from multiple goroutines.
package testevals
