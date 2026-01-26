/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"reflect"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Global metrics with consistent dimensions
	evaluationCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_evaluations_total",
			Help: "Total number of agent evaluations performed",
		},
		[]string{"tracer_type", "namespace"},
	)

	failureCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_evaluation_failures_total",
			Help: "Total number of failed evaluations",
		},
		[]string{"tracer_type", "namespace"},
	)

	gradeGauge = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agent_evaluation_grade",
			Help: "Most recent evaluation grade (0.0-1.0)",
		},
		[]string{"tracer_type", "namespace"},
	)
)

// MetricsObserver implements Observer interface with Prometheus metrics
type MetricsObserver struct {
	tracerType string
	namespace  string

	// Prometheus metrics with labels
	evalCounter prometheus.Counter
	failCounter prometheus.Counter
	gradeGauge  prometheus.Gauge
}

// NewMetricsObserver creates a metrics observer for the given tracer type and namespace
func NewMetricsObserver[T any](namespace string) *MetricsObserver {
	tracerType := reflect.TypeFor[T]().String()
	return &MetricsObserver{
		tracerType: tracerType,
		namespace:  namespace,
		evalCounter: evaluationCounter.With(prometheus.Labels{
			"tracer_type": tracerType,
			"namespace":   namespace,
		}),
		failCounter: failureCounter.With(prometheus.Labels{
			"tracer_type": tracerType,
			"namespace":   namespace,
		}),
		gradeGauge: gradeGauge.With(prometheus.Labels{
			"tracer_type": tracerType,
			"namespace":   namespace,
		}),
	}
}

// Increment implements Observer.Increment
func (m *MetricsObserver) Increment() {
	m.evalCounter.Inc()
}

// Fail implements Observer.Fail
func (m *MetricsObserver) Fail(msg string) {
	m.failCounter.Inc()
}

// Grade implements Observer.Grade
func (m *MetricsObserver) Grade(score float64, reasoning string) {
	m.gradeGauge.Set(score)
}

// Log implements Observer.Log (no-op for metrics observer)
func (m *MetricsObserver) Log(msg string) {
	// No-op: metrics observer doesn't log
}

// Total implements Observer.Total
func (m *MetricsObserver) Total() int64 {
	return 0
}
