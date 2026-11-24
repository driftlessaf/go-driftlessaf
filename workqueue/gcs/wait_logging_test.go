/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestWithScheduledWaitWarningThreshold(t *testing.T) {
	const want = 45 * time.Minute
	w := NewWorkQueue(nil, 1, WithScheduledWaitWarningThreshold(want)).(*wq)
	if w.scheduledWaitWarningThreshold != want {
		t.Fatalf("scheduled wait warning threshold = %v, want %v", w.scheduledWaitWarningThreshold, want)
	}
}

func TestLogHighScheduledWait(t *testing.T) {
	tests := []struct {
		name      string
		wait      time.Duration
		threshold time.Duration
		wantLog   bool
	}{
		{name: "disabled", wait: 2 * time.Hour},
		{name: "below threshold", wait: time.Hour - time.Second, threshold: time.Hour},
		{name: "at threshold", wait: time.Hour, threshold: time.Hour, wantLog: true},
		{name: "above threshold", wait: time.Hour + time.Second, threshold: time.Hour, wantLog: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := clog.New(slog.NewJSONHandler(&buf, nil))
			ctx := clog.WithLogger(t.Context(), logger)

			logHighScheduledWait(ctx, tc.wait, tc.threshold, "octo-org")

			got := buf.String()
			if tc.wantLog != strings.Contains(got, `"event":"workqueue_scheduled_wait_high"`) {
				t.Fatalf("structured wait warning presence = %t, want %t; log: %s", got != "", tc.wantLog, got)
			}
			if tc.wantLog && !strings.Contains(got, `"threshold_seconds":3600`) {
				t.Fatalf("structured wait warning missing threshold: %s", got)
			}
			if tc.wantLog && !strings.Contains(got, `"tenant":"octo-org"`) {
				t.Fatalf("structured wait warning missing tenant: %s", got)
			}
		})
	}
}

func TestCutShort(t *testing.T) {
	const (
		zero   = "0001-01-01T00:00:00Z"
		past   = "2026-11-24T10:00:00Z"
		now    = "2026-11-24T12:00:00Z"
		ahead  = "2026-11-24T12:07:00Z"
		future = "2026-11-24T23:00:00Z"
	)
	for _, tc := range []struct {
		name                    string
		existing, incoming, now string
		want                    bool
	}{
		{name: "an enqueue cuts a pending not-before short", existing: future, incoming: zero, now: now, want: true},
		{name: "a writer clock running ahead still records the cut", existing: future, incoming: zero, now: ahead, want: true},
		{name: "an earlier not-before still in the future", existing: future, incoming: ahead, now: now},
		{name: "a not-before that already passed", existing: past, incoming: zero, now: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutShort(tc.existing, tc.incoming, tc.now); got != tc.want {
				t.Errorf("cutShort(%q, %q, %q): got = %t, want = %t", tc.existing, tc.incoming, tc.now, got, tc.want)
			}
		})
	}
}

func TestWaitStarts(t *testing.T) {
	created := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	requeued := time.Date(2026, 11, 24, 11, 0, 0, 0, time.UTC)
	lastAttempted := strconv.FormatInt(requeued.Unix(), 10)
	at := func(hour int) time.Time { return time.Date(2026, 11, 24, hour, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name                    string
		metadata                map[string]string
		wantWait, wantScheduled time.Time
	}{{
		name:     "fresh key",
		metadata: map[string]string{notBeforeMetadataKey: noNotBefore},
		wantWait: created, wantScheduled: created,
	}, {
		name:     "fresh key with a not-before",
		metadata: map[string]string{notBeforeMetadataKey: "2026-11-24T09:00:00Z"},
		wantWait: created, wantScheduled: at(9),
	}, {
		// Enqueued at 08:00 with a not-before of 07:00: claimable from its
		// creation, not from the not-before.
		name:     "fresh key with a not-before already passed",
		metadata: map[string]string{notBeforeMetadataKey: "2026-11-24T07:00:00Z"},
		wantWait: created, wantScheduled: created,
	}, {
		name:     "requeued with a delay",
		metadata: map[string]string{lastAttemptedKey: lastAttempted, notBeforeMetadataKey: "2026-11-24T12:00:00Z"},
		wantWait: requeued, wantScheduled: at(12),
	}, {
		// Requeued at 11:00 to come back at 23:00, and woken by an enqueue at
		// 13:00, which stored the zero not-before.
		name:     "not-before cut short",
		metadata: map[string]string{lastAttemptedKey: lastAttempted, notBeforeMetadataKey: noNotBefore, notBeforeCutAtKey: "2026-11-24T13:00:00Z"},
		wantWait: requeued, wantScheduled: at(13),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			wait, scheduled := waitStarts(created, tc.metadata)
			if !wait.Equal(tc.wantWait) || !scheduled.Equal(tc.wantScheduled) {
				t.Errorf("waitStarts(): got = %v, %v, want = %v, %v", wait, scheduled, tc.wantWait, tc.wantScheduled)
			}
		})
	}
}

// TestStartDropsCutTimeAndClampsScheduledWait claims a key whose not-before was
// cut short by a writer whose clock runs an hour ahead. The claim must not
// carry the cut time, nor the not-before floor, onto the in-progress object,
// and must observe a scheduled wait of zero rather than a negative one.
func TestStartDropsCutTimeAndClampsScheduledWait(t *testing.T) {
	const queueName = "start-drops-cut-time"
	f := &fakeGCS{handler: copyAndDeleteHandler("in-progress/test-key", 1)}
	qk := &queuedKey{
		client: newTestClient(t, f),
		attrs: &keyAttrs{
			Name: queuedPrefix + "test-key",
			Metadata: map[string]string{
				notBeforeMetadataKey:      noNotBefore,
				notBeforeFloorMetadataKey: notBeforeFloorValue,
				notBeforeCutAtKey:         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		},
		queueName: queueName,
	}
	labels := qk.baseLabels()
	labels["priority_class"] = priorityClass(qk.priority)
	// The histogram is package-level, so compare against what it held before.
	countBefore, sumBefore := scheduledWaitSamples(t, labels)
	if _, err := qk.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	rewrite, ok := findCall(f.recorded(), "POST", "rewriteTo")
	if !ok {
		t.Fatal("no rewrite call recorded")
	}
	md := rewriteMetadata(t, rewrite)
	for _, k := range []string{notBeforeCutAtKey, notBeforeFloorMetadataKey} {
		if v, ok := md[k]; ok {
			t.Errorf("in-progress metadata %s: got = %q, want none", k, v)
		}
	}

	count, sum := scheduledWaitSamples(t, labels)
	if got := count - countBefore; got != 1 {
		t.Fatalf("scheduled wait observations: got = %d, want = 1", got)
	}
	if got := sum - sumBefore; got != 0 {
		t.Errorf("scheduled wait: got = %vs, want = 0s for a cut time after the claim", got)
	}
}

// scheduledWaitSamples returns how many scheduled waits the histogram holds for
// labels, and their sum in seconds.
func scheduledWaitSamples(t *testing.T, labels prometheus.Labels) (uint64, float64) {
	t.Helper()
	var pb dto.Metric
	if err := mWaitLatencyFromScheduled.With(labels).(prometheus.Metric).Write(&pb); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	return pb.GetHistogram().GetSampleCount(), pb.GetHistogram().GetSampleSum()
}
