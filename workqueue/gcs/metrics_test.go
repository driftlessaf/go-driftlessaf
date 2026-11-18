/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"fmt"
	"maps"
	"net/http"
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

func TestEnumerateQueuedKeysByPriorityClass(t *testing.T) {
	const queueName = "queued-by-priority-class-test"
	labels := prometheus.Labels{
		"service_name":  baseServiceName,
		"revision_name": baseRevisionName,
		"queue_name":    queueName,
	}
	classLabels := func(class string) prometheus.Labels {
		l := maps.Clone(labels)
		l["priority_class"] = class
		return l
	}

	// Only eligible queued keys count: the deferred, in-progress and
	// dead-lettered keys at 5xx must not.
	var items string
	f := &fakeGCS{handler: func(gcsCall) (int, string) {
		return http.StatusOK, `{"kind":"storage#objects","items":[` + items + `]}`
	}}
	items = `
		{"kind":"storage#object","bucket":"b","name":"queued/a","generation":"1","metageneration":"1","metadata":{"priority":"00000500"}},
		{"kind":"storage#object","bucket":"b","name":"queued/b","generation":"1","metageneration":"1","metadata":{"priority":"00000542"}},
		{"kind":"storage#object","bucket":"b","name":"queued/c","generation":"1","metageneration":"1"},
		{"kind":"storage#object","bucket":"b","name":"queued/deferred","generation":"1","metageneration":"1","metadata":{"priority":"00000500","not-before":"2999-01-01T00:00:00Z"}},
		{"kind":"storage#object","bucket":"b","name":"in-progress/d","generation":"1","metageneration":"1","metadata":{"priority":"00000500"}},
		{"kind":"storage#object","bucket":"b","name":"dead-letter/e","generation":"1","metageneration":"1","metadata":{"priority":"00000500"}}`
	wq := NewWorkQueue(newTestClient(t, f), 10, WithName(queueName))
	if _, _, _, err := wq.Enumerate(t.Context()); err != nil {
		t.Fatalf("Enumerate() = %v", err)
	}
	if got := testutil.ToFloat64(mQueuedKeysByPriorityClass.With(classLabels("5xx"))); got != 2 {
		t.Errorf("5xx: got = %v, want = 2", got)
	}
	if got := testutil.ToFloat64(mQueuedKeysByPriorityClass.With(classLabels("0xx"))); got != 1 {
		t.Errorf("0xx: got = %v, want = 1", got)
	}
	if got := testutil.ToFloat64(mQueuedKeys.With(labels)); got != 3 {
		t.Errorf("workqueue_queued_keys: got = %v, want = 3", got)
	}

	// A drained class reads 0 rather than vanishing, so a minimum across
	// replicas cannot fall back to a replica still holding an older count.
	items = `{"kind":"storage#object","bucket":"b","name":"queued/c","generation":"1","metageneration":"1"}`
	if _, _, _, err := wq.Enumerate(t.Context()); err != nil {
		t.Fatalf("Enumerate() = %v", err)
	}
	if got := testutil.ToFloat64(mQueuedKeysByPriorityClass.With(classLabels("5xx"))); got != 0 {
		t.Errorf("5xx after drain: got = %v, want = 0", got)
	}
	if got := queueSeries(t, mQueuedKeysByPriorityClass, queueName); got != 2 {
		t.Errorf("series after drain: got = %d, want = 2", got)
	}
}
