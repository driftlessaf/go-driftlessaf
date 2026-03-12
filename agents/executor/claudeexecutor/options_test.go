/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestWithMaxTurns(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	tests := []struct {
		name    string
		turns   int
		wantErr bool
	}{
		{name: "valid turns", turns: 10, wantErr: false},
		{name: "one turn", turns: 1, wantErr: false},
		{name: "large turns", turns: 100, wantErr: false},
		{name: "zero turns", turns: 0, wantErr: true},
		{name: "negative turns", turns: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New[*testBindable, *testResponse](
				anthropic.Client{}, // client not needed for option validation
				prompt,
				WithMaxTurns[*testBindable, *testResponse](tt.turns),
			)

			if (err != nil) != tt.wantErr {
				t.Errorf("WithMaxTurns(%d) error = %v, wantErr %v", tt.turns, err, tt.wantErr)
			}
		})
	}
}

func TestDefaultMaxTurns(t *testing.T) {
	t.Parallel()

	if DefaultMaxTurns <= 0 {
		t.Errorf("DefaultMaxTurns = %d, want > 0", DefaultMaxTurns)
	}
}

func TestMaxTurnsApplied(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	// Without option: should get default
	exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e := exec.(*executor[*testBindable, *testResponse])
	if e.maxTurns != DefaultMaxTurns {
		t.Errorf("default maxTurns = %d, want %d", e.maxTurns, DefaultMaxTurns)
	}

	// With option: should override
	exec2, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt,
		WithMaxTurns[*testBindable, *testResponse](25),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e2 := exec2.(*executor[*testBindable, *testResponse])
	if e2.maxTurns != 25 {
		t.Errorf("custom maxTurns = %d, want 25", e2.maxTurns)
	}
}

// testBindable implements promptbuilder.Bindable for testing.
type testBindable struct{}

func (t *testBindable) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

// testResponse is a simple response type for testing.
type testResponse struct {
	Result string `json:"result"`
}
