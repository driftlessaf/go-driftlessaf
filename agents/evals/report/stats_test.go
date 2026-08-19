/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report

import "testing"

// TestStatsFormatForTree pins the value and label of every formatForTree
// branch. The grade-only label must report the grade count and the iteration
// total separately, because an eval such as NoErrors only grades iterations it
// considers clean, so a grade count equal to the average cannot stand in for
// the number of iterations that ran.
func TestStatsFormatForTree(t *testing.T) {
	tests := []struct {
		name      string
		stats     stats
		wantValue string
		wantLabel string
	}{{
		name: "grade only reports grade count and iteration total",
		stats: stats{
			gradeCount: 3,
			gradeScore: 3.0,
			iterations: 3,
		},
		wantValue: "1.00 avg",
		wantLabel: "(3 grades over 3 iterations)",
	}, {
		name: "grade only uses singular words for one grade and one iteration",
		stats: stats{
			gradeCount: 1,
			gradeScore: 1.0,
			iterations: 1,
		},
		wantValue: "1.00 avg",
		wantLabel: "(1 grade over 1 iteration)",
	}, {
		name: "grade only keeps the counts apart when one iteration emits several grades",
		stats: stats{
			gradeCount: 3,
			gradeScore: 2.4,
			iterations: 1,
		},
		wantValue: "0.80 avg",
		wantLabel: "(3 grades over 1 iteration)",
	}, {
		name: "grade only reports plural grades over a single iteration",
		stats: stats{
			gradeCount: 2,
			gradeScore: 1.0,
			iterations: 1,
		},
		wantValue: "0.50 avg",
		wantLabel: "(2 grades over 1 iteration)",
	}, {
		name: "grades and failures report pass rate and average",
		stats: stats{
			failureCount: 1,
			gradeCount:   2,
			gradeScore:   1.5,
			iterations:   4,
		},
		wantValue: "75.0% pass, 0.75 avg",
		wantLabel: "(3/4)",
	}, {
		name: "failures without grades report the pass rate only",
		stats: stats{
			failureCount: 1,
			iterations:   4,
		},
		wantValue: "75.0%",
		wantLabel: "(3/4)",
	}, {
		name: "no failures and no grades report a full pass rate",
		stats: stats{
			iterations: 4,
		},
		wantValue: "100.0%",
		wantLabel: "(4/4)",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.stats
			s.finalize()

			gotValue, gotLabel := s.formatForTree()

			if gotValue != tt.wantValue {
				t.Errorf("value: got = %q, wanted = %q", gotValue, tt.wantValue)
			}
			if gotLabel != tt.wantLabel {
				t.Errorf("label: got = %q, wanted = %q", gotLabel, tt.wantLabel)
			}
		})
	}
}
