/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package effort_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
)

func TestLevelValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level   effort.Level
		wantErr bool
	}{
		{effort.Low, false},
		{effort.Medium, false},
		{effort.High, false},
		{effort.XHigh, false},
		{effort.Max, false},
		{"", true},        // unset is expressed by not passing the option
		{"XHIGH", true},   // case-sensitive
		{"extreme", true}, // not a level
	}
	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			err := tt.level.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%q) err = %v, wantErr %v", tt.level, err, tt.wantErr)
			}
		})
	}
}
