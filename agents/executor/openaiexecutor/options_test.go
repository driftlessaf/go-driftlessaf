/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

import (
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go"
)

type testRequest struct{}

func (r *testRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type testResponse struct{}

func TestWithModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		model   string
		wantErr string
	}{{
		name:  "valid model",
		model: "google/gemini-2.5-pro",
	}, {
		name:    "empty model",
		model:   "",
		wantErr: "cannot be empty",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opt := WithModel[*testRequest, testResponse](tt.model)
			err := opt(&executor[*testRequest, testResponse]{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("WithModel(%q): got = %v, wanted error containing %q", tt.model, err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("WithModel(%q): got = %v, wanted = nil", tt.model, err)
			}
		})
	}
}

func TestWithTemperature(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		temp    float64
		wantErr bool
	}{{
		name: "valid low",
		temp: 0.0,
	}, {
		name: "valid high",
		temp: 2.0,
	}, {
		name: "valid mid",
		temp: 0.7,
	}, {
		name:    "too low",
		temp:    -0.1,
		wantErr: true,
	}, {
		name:    "too high",
		temp:    2.1,
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opt := WithTemperature[*testRequest, testResponse](tt.temp)
			err := opt(&executor[*testRequest, testResponse]{})
			if tt.wantErr && err == nil {
				t.Errorf("WithTemperature(%f): got = nil, wanted = error", tt.temp)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("WithTemperature(%f): got = %v, wanted = nil", tt.temp, err)
			}
		})
	}
}

func TestWithMaxTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tokens  int64
		wantErr bool
	}{{
		name:   "valid",
		tokens: 8192,
	}, {
		name:    "zero",
		tokens:  0,
		wantErr: true,
	}, {
		name:    "negative",
		tokens:  -1,
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opt := WithMaxTokens[*testRequest, testResponse](tt.tokens)
			err := opt(&executor[*testRequest, testResponse]{})
			if tt.wantErr && err == nil {
				t.Errorf("WithMaxTokens(%d): got = nil, wanted = error", tt.tokens)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("WithMaxTokens(%d): got = %v, wanted = nil", tt.tokens, err)
			}
		})
	}
}

func TestWithMaxTurns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		turns   int
		wantErr bool
	}{{
		name:  "valid",
		turns: 10,
	}, {
		name:    "zero",
		turns:   0,
		wantErr: true,
	}, {
		name:    "negative",
		turns:   -1,
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opt := WithMaxTurns[*testRequest, testResponse](tt.turns)
			err := opt(&executor[*testRequest, testResponse]{})
			if tt.wantErr && err == nil {
				t.Errorf("WithMaxTurns(%d): got = nil, wanted = error", tt.turns)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("WithMaxTurns(%d): got = %v, wanted = nil", tt.turns, err)
			}
		})
	}
}

func TestWithSystemInstructions(t *testing.T) {
	t.Parallel()

	opt := WithSystemInstructions[*testRequest, testResponse](nil)
	err := opt(&executor[*testRequest, testResponse]{})
	if err == nil {
		t.Error("WithSystemInstructions(nil): got = nil, wanted = error")
	}
}

func TestWithUserPromptSuffix(t *testing.T) {
	t.Parallel()

	// Nil suffix: option must fail.
	opt := WithUserPromptSuffix[*testRequest, testResponse](nil)
	if err := opt(&executor[*testRequest, testResponse]{}); err == nil {
		t.Error("WithUserPromptSuffix(nil): got = nil, wanted = error")
	}

	// Valid suffix: stored on the executor for concatenation at Execute time.
	suffix, err := promptbuilder.NewPrompt("lens suffix body")
	if err != nil {
		t.Fatalf("NewPrompt(suffix) error = %v", err)
	}
	e := &executor[*testRequest, testResponse]{}
	if err := WithUserPromptSuffix[*testRequest, testResponse](suffix)(e); err != nil {
		t.Fatalf("WithUserPromptSuffix(): got = %v, wanted = nil", err)
	}
	if got, want := e.userPromptSuffix, suffix; got != want {
		t.Errorf("userPromptSuffix: got = %p, want = %p", got, want)
	}
}

// TestAppendUserPromptSuffix asserts the exact concatenation Execute performs
// on the built user prompt: nil suffix passes the prompt through unchanged, a
// bound suffix is appended with a blank-line separator, and an unbuildable
// suffix (unbound placeholder) surfaces as an error.
func TestAppendUserPromptSuffix(t *testing.T) {
	t.Parallel()

	suffix, err := promptbuilder.NewPrompt("lens suffix body")
	if err != nil {
		t.Fatalf("NewPrompt(suffix) error = %v", err)
	}
	unbuildable, err := promptbuilder.NewPrompt("{{unbound}}")
	if err != nil {
		t.Fatalf("NewPrompt(unbuildable) error = %v", err)
	}

	tests := []struct {
		name    string
		suffix  *promptbuilder.Prompt
		want    string
		wantErr bool
	}{
		{name: "nil suffix passes prompt through", suffix: nil, want: "changeset payload"},
		{name: "suffix appended with blank-line separator", suffix: suffix, want: "changeset payload\n\nlens suffix body"},
		{name: "unbuildable suffix returns error", suffix: unbuildable, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := appendUserPromptSuffix("changeset payload", tt.suffix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("appendUserPromptSuffix() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Errorf("appendUserPromptSuffix(): got = %q, want = %q", got, tt.want)
			}
		})
	}
}

func TestWithSubmitResultProvider_Nil(t *testing.T) {
	t.Parallel()

	opt := WithSubmitResultProvider[*testRequest, testResponse](nil)
	err := opt(&executor[*testRequest, testResponse]{})
	if err == nil {
		t.Error("WithSubmitResultProvider(nil): got = nil, wanted = error")
	}
}

func TestWithSubmitResultProvider_Error(t *testing.T) {
	t.Parallel()

	provider := func() (openaistool.SubmitMetadata[testResponse], error) {
		return openaistool.SubmitMetadata[testResponse]{}, errors.New("provider failed")
	}
	opt := WithSubmitResultProvider[*testRequest, testResponse](provider)
	err := opt(&executor[*testRequest, testResponse]{})
	if err == nil {
		t.Error("WithSubmitResultProvider(erroring provider): got = nil, wanted = error")
	}
}

func TestNew_NilPrompt(t *testing.T) {
	t.Parallel()

	_, err := New[*testRequest, testResponse](openai.Client{}, nil)
	if err == nil {
		t.Error("New(nil prompt): got = nil, wanted = error")
	}
}
