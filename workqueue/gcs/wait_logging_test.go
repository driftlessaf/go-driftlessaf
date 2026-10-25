/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chainguard-dev/clog"
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

			logHighScheduledWait(ctx, tc.wait, tc.threshold)

			got := buf.String()
			if tc.wantLog != strings.Contains(got, `"event":"workqueue_scheduled_wait_high"`) {
				t.Fatalf("structured wait warning presence = %t, want %t; log: %s", got != "", tc.wantLog, got)
			}
			if tc.wantLog && !strings.Contains(got, `"threshold_seconds":3600`) {
				t.Fatalf("structured wait warning missing threshold: %s", got)
			}
		})
	}
}
