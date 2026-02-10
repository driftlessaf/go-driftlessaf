/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals_test

import (
	"context"
	"fmt"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
)

// testNamedObserver implements Observer for testing NamespacedObserver
type testNamedObserver struct {
	name     string
	failures []string
	logs     []string
	count    int64
}

func (t *testNamedObserver) Fail(msg string) {
	t.failures = append(t.failures, msg)
}

func (t *testNamedObserver) Log(msg string) {
	t.logs = append(t.logs, msg)
}

func (t *testNamedObserver) Grade(score float64, reasoning string) {
	t.logs = append(t.logs, fmt.Sprintf("Grade: %.2f - %s", score, reasoning))
}

func (t *testNamedObserver) Increment() {
	t.count++
}

func (t *testNamedObserver) Total() int64 {
	return t.count
}

func TestNewNamespacedObserver(t *testing.T) {
	// Track the created observer to verify delegation
	var createdObserver *testNamedObserver

	// Create a factory function that creates testNamedObserver instances
	factory := func(name string) *testNamedObserver {
		createdObserver = &testNamedObserver{
			name: name,
		}
		return createdObserver
	}

	// Create a new NamespacedObserver
	root := evals.NewNamespacedObserver(factory)

	// Verify the root node was created correctly
	if root == nil {
		t.Fatal("NewNamespacedObserver returned nil")
	}

	// Verify the factory was called with correct name for root
	if createdObserver == nil {
		t.Fatal("Factory was not called")
	}
	if createdObserver.name != "/" {
		t.Errorf("root observer name: got = %q, wanted = '/'", createdObserver.name)
	}

	// Test that Log delegates to inner
	root.Log("test log message")
	if len(createdObserver.logs) != 1 {
		t.Errorf("log count: got = %d, wanted = 1", len(createdObserver.logs))
	} else if createdObserver.logs[0] != "test log message" {
		t.Errorf("log message: got = %q, wanted = %q", createdObserver.logs[0], "test log message")
	}

	// Test that Fail delegates to inner
	root.Fail("test failure")
	if len(createdObserver.failures) != 1 {
		t.Errorf("failure count: got = %d, wanted = 1", len(createdObserver.failures))
	} else if createdObserver.failures[0] != "test failure" {
		t.Errorf("failure message: got = %q, wanted = %q", createdObserver.failures[0], "test failure")
	}

	// Test that Increment delegates to inner
	root.Increment()
	if createdObserver.count != 1 {
		t.Errorf("count: got = %d, wanted = 1", createdObserver.count)
	}

	// Test that Total delegates to inner
	if total := root.Total(); total != 1 {
		t.Errorf("total: got = %d, wanted = 1", total)
	}

	// Test that Grade delegates to inner
	root.Grade(0.85, "good performance")
	expectedGrade := "Grade: 0.85 - good performance"
	if len(createdObserver.logs) != 2 {
		t.Errorf("log count after grade: got = %d, wanted = 2", len(createdObserver.logs))
	} else if createdObserver.logs[1] != expectedGrade {
		t.Errorf("grade message: got = %q, wanted = %q", createdObserver.logs[1], expectedGrade)
	}
}

func TestNamespacedObserverAsObserver(t *testing.T) {
	// Create a factory that tracks messages
	factory := func(name string) *testNamedObserver {
		return &testNamedObserver{
			name: name,
		}
	}

	// Create a NamespacedObserver
	observer := evals.NewNamespacedObserver(factory)

	// Use it as an Observer with an ObservableTraceCallback
	callback := func(o evals.Observer, trace *agenttrace.Trace[string]) {
		o.Log("Processing trace: " + trace.InputPrompt)
		if trace.Error != nil {
			o.Fail("Trace failed: " + trace.Error.Error())
		}
	}

	// Inject the NamespacedObserver and pass to ByCode tracer
	traceCallback := evals.Inject[string](observer, callback)
	tracer := agenttrace.ByCode[string](traceCallback)

	// Create and complete a trace - this will automatically invoke the callback
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Test input")
	trace.Complete("test result", nil)

	// The test passes if no panic occurs
}

func TestNamespacedObserverChild(t *testing.T) {
	// Track created instances
	createdInstances := make(map[string]*testNamedObserver, 3)

	// Create a factory that tracks what names are used
	factory := func(name string) *testNamedObserver {
		obs := &testNamedObserver{
			name: name,
		}
		createdInstances[name] = obs
		return obs
	}

	// Create root observer
	root := evals.NewNamespacedObserver(factory)

	// Verify root was created with "/"
	if _, ok := createdInstances["/"]; !ok {
		t.Error("Root observer not created with '/' name")
	}

	// Get a child
	child1 := root.Child("subtest1")

	// Verify child was created with correct path
	if _, ok := createdInstances["/subtest1"]; !ok {
		t.Error("Child not created with expected path '/subtest1'")
	}

	// Get the same child again - should return same instance
	child1Again := root.Child("subtest1")
	if child1 != child1Again {
		t.Error("child instance: got = different instance, wanted = same instance for same name")
	}

	// Verify only 2 instances were created (root + child1)
	if len(createdInstances) != 2 {
		t.Errorf("created instances: got = %d, wanted = 2", len(createdInstances))
	}

	// Create a grandchild
	grandchild := child1.Child("nested")

	// Verify grandchild has correct path
	if _, ok := createdInstances["/subtest1/nested"]; !ok {
		t.Error("Grandchild not created with expected path '/subtest1/nested'")
	}

	// Test that each namespace logs independently
	root.Log("root log")
	child1.Log("child1 log")
	grandchild.Log("grandchild log")

	// Verify each logged to its own instance
	rootObs := createdInstances["/"]
	if len(rootObs.logs) != 1 || rootObs.logs[0] != "root log" {
		t.Errorf("Root log incorrect: %v", rootObs.logs)
	}

	child1Obs := createdInstances["/subtest1"]
	if len(child1Obs.logs) != 1 || child1Obs.logs[0] != "child1 log" {
		t.Errorf("Child1 log incorrect: %v", child1Obs.logs)
	}

	grandchildObs := createdInstances["/subtest1/nested"]
	if len(grandchildObs.logs) != 1 || grandchildObs.logs[0] != "grandchild log" {
		t.Errorf("Grandchild log incorrect: %v", grandchildObs.logs)
	}
}

func TestNamespacedObserverWalk(t *testing.T) {
	// Track walk order
	var walkOrder []string
	var walkInstances []string

	// Create a factory
	factory := func(name string) *testNamedObserver {
		return &testNamedObserver{
			name: name,
		}
	}

	// Create root and build a tree
	root := evals.NewNamespacedObserver(factory)

	// Create children in non-alphabetical order to test sorting
	child2 := root.Child("beta")
	child1 := root.Child("alpha")
	root.Child("gamma")

	// Create some grandchildren
	child1.Child("a1")
	child1.Child("a2")
	child2.Child("b1")

	// Walk the tree and collect the order
	root.Walk(func(name string, obs *testNamedObserver) {
		walkOrder = append(walkOrder, name)
		walkInstances = append(walkInstances, obs.name)
	})

	// Verify walk order is correct (depth-first, sorted children)
	expectedOrder := []string{
		"/",         // root first
		"/alpha",    // children in sorted order
		"/alpha/a1", // alpha's children
		"/alpha/a2",
		"/beta",
		"/beta/b1", // beta's child
		"/gamma",   // gamma (no children)
	}

	if len(walkOrder) != len(expectedOrder) {
		t.Fatalf("Walk visited wrong number of nodes: got %d, want %d", len(walkOrder), len(expectedOrder))
	}

	for i, expected := range expectedOrder {
		if walkOrder[i] != expected {
			t.Errorf("Walk order[%d]: got = %q, wanted = %q", i, walkOrder[i], expected)
		}
		// Also verify the instance name matches
		if walkInstances[i] != expected {
			t.Errorf("Walk instance[%d]: got = %q, wanted = %q", i, walkInstances[i], expected)
		}
	}
}

func TestNamespacedObserverWalkEmpty(t *testing.T) {
	// Test walking a tree with just root
	visitCount := 0

	factory := func(name string) *testNamedObserver {
		return &testNamedObserver{name: name}
	}

	root := evals.NewNamespacedObserver(factory)

	root.Walk(func(name string, obs *testNamedObserver) {
		visitCount++
		if name != "/" {
			t.Errorf("visit name: got = %q, wanted = '/'", name)
		}
	})

	if visitCount != 1 {
		t.Errorf("visit count: got = %d, wanted = 1", visitCount)
	}
}
