/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue_test

import (
	"testing"

	"chainguard.dev/driftlessaf/workqueue"
)

func TestAttemptFinal(t *testing.T) {
	tests := []struct {
		name    string
		attempt workqueue.Attempt
		want    bool
	}{
		{name: "retries left", attempt: workqueue.Attempt{Number: 2, Max: 3}},
		{name: "at the budget", attempt: workqueue.Attempt{Number: 3, Max: 3}, want: true},
		{name: "over the budget", attempt: workqueue.Attempt{Number: 4, Max: 3}, want: true},
		{name: "unbounded", attempt: workqueue.Attempt{Number: 40}},
		{name: "unreported", attempt: workqueue.Attempt{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.attempt.Final(); got != tt.want {
				t.Errorf("Final(): got = %t, want = %t", got, tt.want)
			}
		})
	}
}
