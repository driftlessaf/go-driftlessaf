/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import "testing"

func TestScoreAnswerLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		probabilities map[string]float64
		wantKey       string
		wantP         float64
	}{
		{name: "empty", probabilities: nil},
		{name: "single", probabilities: map[string]float64{"0": 1}, wantKey: "0", wantP: 1},
		{name: "max wins", probabilities: map[string]float64{"0": 0.1, "1": 0.6, "2": 0.3}, wantKey: "1", wantP: 0.6},
		{name: "tie resolves to smallest key", probabilities: map[string]float64{"2": 0.5, "1": 0.5}, wantKey: "1", wantP: 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, p := ScoreAnswer{Probabilities: tt.probabilities}.Level()
			if key != tt.wantKey || p != tt.wantP {
				t.Errorf("Level(): got = (%q, %v), want = (%q, %v)", key, p, tt.wantKey, tt.wantP)
			}
		})
	}
}

func TestAnswerKind(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		answer Answer
		want   string
	}{
		{answer: NoulAnswer{}, want: "noul"},
		{answer: ChoiceAnswer{}, want: "choice"},
		{answer: ScoreAnswer{}, want: "score"},
	} {
		if got := tt.answer.Kind(); got != tt.want {
			t.Errorf("%T.Kind(): got = %q, want = %q", tt.answer, got, tt.want)
		}
	}
}
