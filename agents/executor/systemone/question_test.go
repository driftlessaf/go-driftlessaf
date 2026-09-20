/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import (
	"encoding/json"
	"testing"
)

func TestQuestionMarshalJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		question Question
		want     string
	}{
		{
			name:     "noul without criteria",
			question: Noul{Instructions: "Is it raining?"},
			want:     `{"type":"noul","instructions":"Is it raining?"}`,
		},
		{
			name:     "noul with one criterion",
			question: Noul{Instructions: "Is it raining?", True: "water falls from the sky"},
			want:     `{"type":"noul","instructions":"Is it raining?","criteria":{"true":"water falls from the sky"}}`,
		},
		{
			name:     "noul with structured instructions",
			question: Noul{Instructions: map[string]any{"ask": "raining", "scope": []string{"now"}}, False: "dry"},
			want:     `{"type":"noul","instructions":{"ask":"raining","scope":["now"]},"criteria":{"false":"dry"}}`,
		},
		{
			name:     "choice keeps nil descriptions as null",
			question: Choice{Instructions: "Pick.", Options: map[string]Content{"a": nil, "b": "second"}},
			want:     `{"type":"choice","instructions":"Pick.","criteria":{"a":null,"b":"second"}}`,
		},
		{
			name:     "score keeps level order",
			question: Score{Instructions: "Rate.", Levels: []Content{"low", "mid", "high"}},
			want:     `{"type":"score","instructions":"Rate.","criteria":["low","mid","high"]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tt.question)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal: got = %s, want = %s", got, tt.want)
			}
		})
	}
}

func TestQuestionValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		question Question
		wantErr  bool
	}{
		{name: "noul ok", question: Noul{Instructions: "i"}},
		{name: "noul nil instructions", question: Noul{}, wantErr: true},
		{name: "choice ok", question: Choice{Instructions: "i", Options: map[string]Content{"a": nil, "b": nil}}},
		{name: "choice nil instructions", question: Choice{Options: map[string]Content{"a": nil, "b": nil}}, wantErr: true},
		{name: "choice one option", question: Choice{Instructions: "i", Options: map[string]Content{"a": nil}}, wantErr: true},
		{name: "choice empty label", question: Choice{Instructions: "i", Options: map[string]Content{"": nil, "b": nil}}, wantErr: true},
		{name: "score ok", question: Score{Instructions: "i", Levels: []Content{"a", "b"}}},
		{name: "score nil instructions", question: Score{Levels: []Content{"a", "b"}}, wantErr: true},
		{name: "score one level", question: Score{Instructions: "i", Levels: []Content{"a"}}, wantErr: true},
		{name: "score nil level", question: Score{Instructions: "i", Levels: []Content{"a", nil}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.question.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate: got = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestQuestionKind(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		question Question
		want     string
	}{
		{question: Noul{}, want: "noul"},
		{question: Choice{}, want: "choice"},
		{question: Score{}, want: "score"},
	} {
		if got := tt.question.Kind(); got != tt.want {
			t.Errorf("%T.Kind(): got = %q, want = %q", tt.question, got, tt.want)
		}
	}
}
