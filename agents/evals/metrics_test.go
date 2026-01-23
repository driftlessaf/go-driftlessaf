/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

type TestResult struct {
	Value string
}

func TestMetricsObserver(t *testing.T) {
	// Create metrics observer
	observer := NewMetricsObserver[*TestResult]("/test/namespace")

	// Test initial state
	if observer.Total() != 0 {
		t.Errorf("Expected initial count to be 0, got %d", observer.Total())
	}

	// Test increment
	observer.Increment()

	// Test grade
	observer.Grade(0.85, "good result")

	// Test fail
	observer.Fail("test failure")

	// Test log (should be no-op)
	observer.Log("test log message")

	// Verify metrics were recorded
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Find our metrics
	var evalTotal, failTotal float64
	var gradeValue float64
	var foundEval, foundFail, foundGrade bool

	for _, family := range families {
		for _, metric := range family.GetMetric() {
			// Check if this metric has our labels
			hasNamespace := false
			hasTracerType := false

			for _, label := range metric.GetLabel() {
				if label.GetName() == "namespace" && label.GetValue() == "/test/namespace" {
					hasNamespace = true
				}
				if label.GetName() == "tracer_type" && label.GetValue() == "*evals.TestResult" {
					hasTracerType = true
				}
			}

			if hasNamespace && hasTracerType {
				switch family.GetName() {
				case "agent_evaluations_total":
					evalTotal = metric.GetCounter().GetValue()
					foundEval = true
				case "agent_evaluation_failures_total":
					failTotal = metric.GetCounter().GetValue()
					foundFail = true
				case "agent_evaluation_grade":
					gradeValue = metric.GetGauge().GetValue()
					foundGrade = true
				}
			}
		}
	}

	if !foundEval {
		t.Error("Evaluation counter metric not found")
	} else if evalTotal != 1 {
		t.Errorf("Expected evaluation counter to be 1, got %f", evalTotal)
	}

	if !foundFail {
		t.Error("Failure counter metric not found")
	} else if failTotal != 1 {
		t.Errorf("Expected failure counter to be 1, got %f", failTotal)
	}

	if !foundGrade {
		t.Error("Grade gauge metric not found")
	} else if gradeValue != 0.85 {
		t.Errorf("Expected grade gauge to be 0.85, got %f", gradeValue)
	}
}

func TestMetricsObserver_TracerTypeFormatting(t *testing.T) {
	tests := []struct {
		name         string
		observerFunc func() Observer
		expectedType string
	}{{
		name: "simple struct pointer",
		observerFunc: func() Observer {
			return NewMetricsObserver[*TestResult]("/test")
		},
		expectedType: "*evals.TestResult",
	}, {
		name: "string pointer",
		observerFunc: func() Observer {
			return NewMetricsObserver[*string]("/test")
		},
		expectedType: "*string",
	}, {
		name: "interface",
		observerFunc: func() Observer {
			return NewMetricsObserver[Observer]("/test")
		},
		expectedType: "evals.Observer",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			observer := tc.observerFunc().(*MetricsObserver)
			if observer.tracerType != tc.expectedType {
				t.Errorf("Expected tracer type %q, got %q", tc.expectedType, observer.tracerType)
			}
		})
	}
}

func TestMetricsObserver_Concurrency(t *testing.T) {
	observer := NewMetricsObserver[*TestResult]("/concurrent/test")

	// Test concurrent access to Increment()
	done := make(chan bool, 100)

	// Start 100 goroutines incrementing
	for range 100 {
		go func() {
			observer.Increment()
			done <- true
		}()
	}

	// Wait for all increments
	for range 100 {
		<-done
	}

	// Total always returns 0 for MetricsObserver
	if observer.Total() != 0 {
		t.Errorf("Expected total to be 0, got %d", observer.Total())
	}
}
