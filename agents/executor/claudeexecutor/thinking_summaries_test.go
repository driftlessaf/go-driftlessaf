/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
)

// sentThinking is the part of a request body TestThinkingSummaries checks.
type sentThinking struct {
	Thinking    *map[string]any `json:"thinking"`
	Temperature *float64        `json:"temperature"`
	ToolChoice  *map[string]any `json:"tool_choice"`
}

// TestThinkingSummaries checks the thinking param of the request actually
// sent: summarized adaptive thinking is on by default for models whose
// provider facts set OmitToolReasoning, WithThinkingSummaries overrides that
// either way, and WithThinking keeps its own mapping. A forced submit
// tool_choice is sent alongside summarized thinking.
func TestThinkingSummaries(t *testing.T) {
	type opt = claudeexecutor.Option[errCapRequest, errCapResponse]
	summarized := map[string]any{"type": "adaptive", "display": "summarized"}
	tests := []struct {
		name            string
		model           string
		opts            []opt
		wantThinking    map[string]any
		wantTemperature *float64
		wantToolChoice  map[string]any
	}{{
		name:         "opus 5.5 on by default",
		model:        "claude-opus-5-5",
		wantThinking: summarized,
	}, {
		name:         "haiku 5.5 on by default",
		model:        "claude-haiku-5-5",
		wantThinking: summarized,
	}, {
		name:            "older model off by default",
		model:           "claude-sonnet-4-6",
		wantTemperature: new(0.1),
	}, {
		name:            "older model with WithToolReasoning(false) stays off",
		model:           "claude-sonnet-4-6",
		opts:            []opt{claudeexecutor.WithToolReasoning[errCapRequest, errCapResponse](false)},
		wantTemperature: new(0.1),
	}, {
		name:  "opus 5.5 with WithThinkingSummaries(false) is off",
		model: "claude-opus-5-5",
		opts:  []opt{claudeexecutor.WithThinkingSummaries[errCapRequest, errCapResponse](false)},
	}, {
		name:            "older model with WithThinkingSummaries(true) is on at temperature 1",
		model:           "claude-sonnet-4-6",
		opts:            []opt{claudeexecutor.WithThinkingSummaries[errCapRequest, errCapResponse](true)},
		wantThinking:    summarized,
		wantTemperature: new(1.0),
	}, {
		name:         "WithThinking on opus 5.5 maps to adaptive",
		model:        "claude-opus-5-5",
		opts:         []opt{claudeexecutor.WithThinking[errCapRequest, errCapResponse](4096)},
		wantThinking: summarized,
	}, {
		name:  "WithThinking on an older model sends its budget",
		model: "claude-sonnet-4-6",
		opts: []opt{
			claudeexecutor.WithThinking[errCapRequest, errCapResponse](4096),
			claudeexecutor.WithThinkingSummaries[errCapRequest, errCapResponse](true),
		},
		wantThinking:    map[string]any{"type": "enabled", "budget_tokens": float64(4096)},
		wantTemperature: new(1.0),
	}, {
		name:           "haiku 5.5 forced submit keeps summarized thinking",
		model:          "claude-haiku-5-5",
		opts:           []opt{claudeexecutor.WithForceSubmitToolChoice[errCapRequest, errCapResponse]("")},
		wantThinking:   summarized,
		wantToolChoice: map[string]any{"type": "tool", "name": "submit_result"},
	}, {
		name:  "older model opted in keeps a forced submit",
		model: "claude-sonnet-4-6",
		opts: []opt{
			claudeexecutor.WithThinkingSummaries[errCapRequest, errCapResponse](true),
			claudeexecutor.WithForceSubmitToolChoice[errCapRequest, errCapResponse](""),
		},
		wantThinking:    summarized,
		wantTemperature: new(1.0),
		wantToolChoice:  map[string]any{"type": "tool", "name": "submit_result"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var first []byte
			srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
				if n == 1 {
					mu.Lock()
					first = body
					mu.Unlock()
				}
				return wrapTurn("msg_01", toolUseBlockSSE(t, 0, "toolu_submit", "submit_result",
					map[string]any{"result": map[string]any{"answer": "done"}, "reasoning": "complete"})...)
			})

			prompt, err := promptbuilder.NewPrompt("go")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			opts := append([]opt{
				claudeexecutor.WithModel[errCapRequest, errCapResponse](tt.model),
				claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](16384),
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
				// Keep the reasoning parameter on every model so one scripted
				// submit matches the schema in every case.
				claudeexecutor.WithToolReasoning[errCapRequest, errCapResponse](true),
				claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
			}, tt.opts...)
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
				anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
				prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := exec.Execute(t.Context(), errCapRequest{}, nil); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			var got sentThinking
			if err := json.Unmarshal(first, &got); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if diff := cmp.Diff(tt.wantThinking, deref(got.Thinking)); diff != "" {
				t.Errorf("thinking (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantTemperature, got.Temperature); diff != "" {
				t.Errorf("temperature (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantToolChoice, deref(got.ToolChoice)); diff != "" {
				t.Errorf("tool_choice (-want, +got):\n%s", diff)
			}
		})
	}
}

func deref(m *map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return *m
}

// TestThinkingSummariesNoAdaptiveThinking checks that New refuses
// WithThinkingSummaries(true) on a model that does not accept adaptive
// thinking, unless WithThinking configures thinking itself.
func TestThinkingSummariesNoAdaptiveThinking(t *testing.T) {
	type opt = claudeexecutor.Option[errCapRequest, errCapResponse]
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	tests := []struct {
		name    string
		model   string
		opts    []opt
		wantErr bool
	}{{
		name:    "haiku 4.5 refused",
		model:   "claude-haiku-4-5",
		wantErr: true,
	}, {
		name:    "haiku 4.5 on Bedrock refused",
		model:   "anthropic.claude-haiku-4-5",
		wantErr: true,
	}, {
		name:    "opus 4.5 refused",
		model:   "claude-opus-4-5@20251101",
		wantErr: true,
	}, {
		name:  "WithThinking on haiku 4.5 takes precedence",
		model: "claude-haiku-4-5",
		opts:  []opt{claudeexecutor.WithThinking[errCapRequest, errCapResponse](4096)},
	}, {
		name:  "sonnet 4.6 accepted",
		model: "claude-sonnet-4-6",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := append([]opt{
				claudeexecutor.WithModel[errCapRequest, errCapResponse](tt.model),
				claudeexecutor.WithThinkingSummaries[errCapRequest, errCapResponse](true),
			}, tt.opts...)
			_, err := claudeexecutor.New[errCapRequest, errCapResponse](anthropic.NewClient(option.WithAPIKey("test")), prompt, opts...)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("New error: got = %v, want error = %t", err, tt.wantErr)
			}
		})
	}
}
