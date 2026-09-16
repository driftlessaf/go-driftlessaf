/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPriorityClass(t *testing.T) {
	tests := []struct {
		priority int64
		want     string
	}{
		{priority: 0, want: "0xx"},
		{priority: 1, want: "0xx"},
		{priority: 99, want: "0xx"},
		{priority: 100, want: "1xx"},
		{priority: 199, want: "1xx"},
		{priority: 200, want: "2xx"},
		{priority: 999, want: "9xx"},
		{priority: 1000, want: "10xx"},
		{priority: -1, want: "0xx"},
		{priority: -100, want: "-1xx"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("priority_%d", tt.priority), func(t *testing.T) {
			got := priorityClass(tt.priority)
			if got != tt.want {
				t.Errorf("priorityClass(%d) = %q, want %q", tt.priority, got, tt.want)
			}
		})
	}
}

func TestEnumerateMaxAttemptsUsesBoundedGauge(t *testing.T) {
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		return 200, `{"kind":"storage#objects","items":[
			{"kind":"storage#object","bucket":"test-bucket","name":"queued/low","generation":"1","metageneration":"1","metadata":{"attempts":"7"}},
			{"kind":"storage#object","bucket":"test-bucket","name":"in-progress/high","generation":"1","metageneration":"1","metadata":{"attempts":"25"}},
			{"kind":"storage#object","bucket":"test-bucket","name":"dead-letter/higher","generation":"1","metageneration":"1","metadata":{"attempts":"100"}}
		]}`
	}}
	const queueName = "bounded-max-attempts-test"
	wq := NewWorkQueue(newTestClient(t, f), 10, WithName(queueName))
	if _, _, _, err := wq.Enumerate(t.Context()); err != nil {
		t.Fatalf("Enumerate() = %v", err)
	}

	labels := prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    queueName,
	}
	if got := testutil.ToFloat64(mMaxAttempts.With(labels)); got != 25 {
		t.Errorf("workqueue_max_attempts = %v, want 25", got)
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather() = %v", err)
	}
	for _, family := range families {
		if family.GetName() == "workqueue_task_max_attempts" {
			t.Fatal("workqueue_task_max_attempts must not be registered")
		}
	}
}
