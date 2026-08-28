/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

import (
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go"
)

type testRequest struct{}

func (r *testRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type testResponse struct{}

func TestWithAttribution(t *testing.T) {
	t.Parallel()

	want := agenttrace.Attribution{
		ProviderName: "test.provider",
		System:       "test.system",
		LogicalModel: "test-model",
		Protocol:     "test-protocol",
	}
	e := &executor[*testRequest, testResponse]{}
	if err := WithAttribution[*testRequest, testResponse](want)(e); err != nil {
		t.Fatalf("WithAttribution: %v", err)
	}
	if e.attribution != want {
		t.Errorf("attribution = %#v, want %#v", e.attribution, want)
	}

	invalid := want
	invalid.LogicalModel = " test-model"
	if err := WithAttribution[*testRequest, testResponse](invalid)(e); err == nil {
		t.Fatal("WithAttribution accepted a logical model with leading whitespace")
	}
}

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

func TestWithProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		provider        Provider
		wantMetricName  string
		wantTraceSystem string
		wantErr         bool
	}{
		{
			name:            "generic OpenAI-compatible",
			provider:        ProviderOpenAICompatible,
			wantMetricName:  "openai-compat",
			wantTraceSystem: agenttrace.SystemOpenAI,
		},
		{
			name:            "Baseten",
			provider:        ProviderBaseten,
			wantMetricName:  "baseten",
			wantTraceSystem: agenttrace.SystemBaseten,
		},
		{
			name:     "unknown provider",
			provider: Provider("bedrock"),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := &executor[*testRequest, testResponse]{
				attribution: agenttrace.Attribution{
					LogicalModel: "fast",
					Protocol:     "openai-chat-completions",
				},
			}
			err := WithProvider[*testRequest, testResponse](tt.provider)(e)
			if tt.wantErr {
				if err == nil {
					t.Fatal("WithProvider: got nil error, wanted validation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("WithProvider: %v", err)
			}
			if got := e.provider.metricName(); got != tt.wantMetricName {
				t.Errorf("metricName: got = %q, want = %q", got, tt.wantMetricName)
			}
			if got := e.provider.traceSystem(); got != tt.wantTraceSystem {
				t.Errorf("traceSystem: got = %q, want = %q", got, tt.wantTraceSystem)
			}
			if got := e.attribution.ProviderName; got != tt.wantMetricName {
				t.Errorf("attribution provider: got = %q, want = %q", got, tt.wantMetricName)
			}
			if got := e.attribution.System; got != tt.wantTraceSystem {
				t.Errorf("attribution system: got = %q, want = %q", got, tt.wantTraceSystem)
			}
			if got := e.attribution.LogicalModel; got != "fast" {
				t.Errorf("logical model changed: got = %q, want = %q", got, "fast")
			}
			if got := e.attribution.Protocol; got != "openai-chat-completions" {
				t.Errorf("protocol changed: got = %q, want = %q", got, "openai-chat-completions")
			}
		})
	}
}

func TestWithTokenLimitParameter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		parameter TokenLimitParameter
		wantErr   bool
	}{
		{name: "max completion tokens", parameter: TokenLimitMaxCompletionTokens},
		{name: "max tokens", parameter: TokenLimitMaxTokens},
		{name: "unknown parameter", parameter: TokenLimitParameter("output_tokens"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := &executor[*testRequest, testResponse]{}
			err := WithTokenLimitParameter[*testRequest, testResponse](tt.parameter)(e)
			if tt.wantErr {
				if err == nil {
					t.Fatal("WithTokenLimitParameter: got nil error, wanted validation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("WithTokenLimitParameter: %v", err)
			}
			if got := e.tokenLimitParam; got != tt.parameter {
				t.Errorf("tokenLimitParam: got = %q, want = %q", got, tt.parameter)
			}
		})
	}
}

func TestNewDefaultsPreserveOpenAICompatibleBehavior(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	got, err := New[*testRequest, testResponse](openai.Client{}, prompt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e, ok := got.(*executor[*testRequest, testResponse])
	if !ok {
		t.Fatalf("New returned %T, want *executor", got)
	}
	if e.provider != ProviderOpenAICompatible {
		t.Errorf("provider: got = %q, want = %q", e.provider, ProviderOpenAICompatible)
	}
	if got, want := e.attribution.ProviderName, "openai-compat"; got != want {
		t.Errorf("default attribution provider: got = %q, want = %q", got, want)
	}
	if got, want := e.attribution.System, agenttrace.SystemOpenAI; got != want {
		t.Errorf("default attribution system: got = %q, want = %q", got, want)
	}
	if e.tokenLimitParam != TokenLimitMaxCompletionTokens {
		t.Errorf("tokenLimitParam: got = %q, want = %q", e.tokenLimitParam, TokenLimitMaxCompletionTokens)
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
