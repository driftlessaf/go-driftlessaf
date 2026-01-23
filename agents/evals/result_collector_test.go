/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
)

func TestResultCollector(t *testing.T) {
	// Create a mock observability
	mock := &exampleObserver{}

	// Create a result collector wrapping the mock
	collector := evals.NewResultCollector(mock)

	// Test that Log passes through
	collector.Log("test log 1")
	collector.Log("test log 2")

	if len(mock.logs) != 2 {
		t.Errorf("logs count: got = %d, wanted = 2", len(mock.logs))
	}
	if mock.logs[0] != "test log 1" || mock.logs[1] != "test log 2" {
		t.Errorf("log messages: got = %v, wanted = [test log 1, test log 2]", mock.logs)
	}

	// Test that Fail logs to inner and is collected
	collector.Fail("failure 1")
	collector.Fail("failure 2")
	collector.Fail("failure 3")

	// Check that failures were logged (not failed) to inner
	if len(mock.logs) != 5 { // 2 from earlier + 3 failures
		t.Errorf("mock logs count: got = %d, wanted = 5 (2 + 3 failures)", len(mock.logs))
	}
	// Inner should have no failures since we log instead
	if len(mock.failures) != 0 {
		t.Errorf("mock failures count: got = %d, wanted = 0", len(mock.failures))
	}

	// Check that failures were collected
	collected := collector.Failures()
	if len(collected) != 3 {
		t.Errorf("collected failures count: got = %d, wanted = 3", len(collected))
	}

	// Verify the failure messages
	expectedFailures := []string{"failure 1", "failure 2", "failure 3"}
	for i, expected := range expectedFailures {
		if collected[i] != expected {
			t.Errorf("Failure[%d]: got = %q, wanted = %q", i, collected[i], expected)
		}
	}

	// Test that Failures() returns a copy
	collected[0] = "modified"
	newCollected := collector.Failures()
	if newCollected[0] == "modified" {
		t.Error("Failures() return type: got = reference to original, wanted = copy")
	}

	// Test Grade functionality
	collector.Grade(0.85, "Good performance")
	collector.Grade(0.95, "Excellent performance")

	// Check that grades were collected
	grades := collector.Grades()
	if len(grades) != 2 {
		t.Errorf("collected grades count: got = %d, wanted = 2", len(grades))
	}

	// Verify the grades
	if grades[0].Score != 0.85 || grades[0].Reasoning != "Good performance" {
		t.Errorf("Grade[0]: got = {%f, %q}, wanted = {0.85, 'Good performance'}", grades[0].Score, grades[0].Reasoning)
	}
	if grades[1].Score != 0.95 || grades[1].Reasoning != "Excellent performance" {
		t.Errorf("Grade[1]: got = {%f, %q}, wanted = {0.95, 'Excellent performance'}", grades[1].Score, grades[1].Reasoning)
	}

	// Test that grades were also logged to inner
	expectedGradeLog1 := "Grade: 0.85 - Good performance"
	expectedGradeLog2 := "Grade: 0.95 - Excellent performance"
	foundGrade1 := false
	foundGrade2 := false
	for _, log := range mock.logs {
		if log == expectedGradeLog1 {
			foundGrade1 = true
		}
		if log == expectedGradeLog2 {
			foundGrade2 = true
		}
	}
	if !foundGrade1 || !foundGrade2 {
		t.Errorf("grade logs: got = missing logs, wanted = all logs found in: %v", mock.logs)
	}

	// Test that Grades() returns a copy
	grades[0].Score = 0.0
	newGrades := collector.Grades()
	if newGrades[0].Score == 0.0 {
		t.Error("Grades() return type: got = reference to original, wanted = copy")
	}
}

func TestResultCollectorConcurrency(t *testing.T) {
	// Create a mock observability
	mock := &exampleObserver{}

	// Create a result collector
	collector := evals.NewResultCollector(mock)

	// Test concurrent access
	var wg sync.WaitGroup

	// Launch multiple goroutines that log and fail
	for range 10 {
		wg.Add(2)

		// Log goroutine
		go func() {
			defer wg.Done()
			for range 10 {
				collector.Log("log message")
			}
		}()

		// Fail goroutine
		go func() {
			defer wg.Done()
			for range 10 {
				collector.Fail("failure message")
			}
		}()
	}

	wg.Wait()

	// Check that all failures were collected
	failures := collector.Failures()
	if len(failures) != 100 { // 10 goroutines * 10 failures each
		t.Errorf("failures count: got = %d, wanted = 100", len(failures))
	}

	// Check that all logs were passed through
	// We expect 200 logs: 100 from Log calls + 100 from Fail calls (which also log)
	if len(mock.logs) != 200 { // (10 goroutines * 10 logs) + (10 goroutines * 10 failures)
		t.Errorf("logs count: got = %d, wanted = 200", len(mock.logs))
	}
}

func TestResultCollectorWithTraceCallback(t *testing.T) {
	// Create a mock observability
	mock := &exampleObserver{}

	// Create a result collector
	collector := evals.NewResultCollector(mock)

	// Use it with a trace callback
	callback := func(o evals.Observer, trace *evals.Trace[string]) {
		o.Log("Processing trace")

		if trace.Error != nil {
			o.Fail("Trace has error: " + trace.Error.Error())
		}

		if len(trace.ToolCalls) == 0 {
			o.Fail("No tool calls found")
		}
	}

	// Create tracer with the evaluation
	tracer := evals.ByCode[string](evals.Inject(collector, callback))
	ctx := context.Background()

	// Test with error trace
	errorTrace := tracer.NewTrace(ctx, "test")
	errorTrace.Complete("failed", errors.New("test error"))

	// Check failures
	failures := collector.Failures()
	if len(failures) != 2 {
		t.Errorf("failures count: got = %d, wanted = 2: %v", len(failures), failures)
	}

	// Test with successful trace
	successTrace := tracer.NewTrace(ctx, "test")
	tc := successTrace.StartToolCall("tc1", "read_logs", nil)
	tc.Complete("log data", nil)
	successTrace.Complete("success", nil)

	// Should still have 2 failures (no new ones)
	failures = collector.Failures()
	if len(failures) != 2 {
		t.Errorf("failures count: got = %d, wanted = 2: %v", len(failures), failures)
	}
}
