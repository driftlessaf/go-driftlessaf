/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestToolReasoning checks which tools a request advertises with the
// reasoning parameter: the model's provider facts decide by default, and
// WithToolReasoning overrides them either way. A hand-built tool keeps its
// own reasoning parameter regardless. Each run also lands a submit shaped
// like the advertised schema, so the submit handler must match it.
func TestToolReasoning(t *testing.T) {
	tests := []struct {
		name          string
		model         string
		opts          []claudeexecutor.Option[errCapRequest, errCapResponse]
		wantReasoning bool
	}{{
		name:          "older model keeps it",
		model:         "claude-sonnet-4-6",
		wantReasoning: true,
	}, {
		name:  "opus 5.5 omits it",
		model: "claude-opus-5-5",
	}, {
		name:  "fable 5.2 omits it",
		model: "claude-fable-5-2@default",
	}, {
		name:          "opus 5.5 with WithToolReasoning(true) keeps it",
		model:         "claude-opus-5-5",
		opts:          []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithToolReasoning[errCapRequest, errCapResponse](true)},
		wantReasoning: true,
	}, {
		name:  "older model with WithToolReasoning(false) omits it",
		model: "claude-sonnet-4-6",
		opts:  []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithToolReasoning[errCapRequest, errCapResponse](false)},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submit := map[string]any{"result": map[string]any{"answer": "done"}}
			if tt.wantReasoning {
				submit["reasoning"] = "complete"
			}
			var mu sync.Mutex
			var first []byte
			srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
				if n == 1 {
					mu.Lock()
					first = body
					mu.Unlock()
				}
				return wrapTurn("msg_01", toolUseBlockSSE(t, 0, "toolu_submit", "submit_result", submit)...)
			})

			prompt, err := promptbuilder.NewPrompt("go")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			opts := append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithModel[errCapRequest, errCapResponse](tt.model),
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
				claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
			}, tt.opts...)
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
				anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
				prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			handler := func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": true}
			}
			tools := map[string]claudetool.Metadata[errCapResponse]{
				"lookup": claudetool.FromTool(toolcall.Tool[errCapResponse]{
					Def:     toolcall.Definition{Name: "lookup", Description: "lookup"},
					Handler: handler,
				}),
				"handmade": {
					Definition: anthropic.ToolParam{
						Name: "handmade",
						InputSchema: anthropic.ToolInputSchemaParam{
							Properties: map[string]any{"reasoning": map[string]any{"type": "string"}},
							Required:   []string{"reasoning"},
						},
					},
				},
			}
			resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got, want := resp.Answer, "done"; got != want {
				t.Errorf("resp.Answer: got = %q, want = %q", got, want)
			}

			mu.Lock()
			defer mu.Unlock()
			var body struct {
				Tools []struct {
					Name        string `json:"name"`
					InputSchema struct {
						Properties map[string]any `json:"properties"`
						Required   []string       `json:"required"`
					} `json:"input_schema"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(first, &body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			for _, tl := range body.Tools {
				want := tt.wantReasoning || tl.Name == "handmade"
				_, hasProp := tl.InputSchema.Properties["reasoning"]
				hasRequired := slices.Contains(tl.InputSchema.Required, "reasoning")
				if hasProp != want || hasRequired != want {
					t.Errorf("tool %q reasoning: got = (property %v, required %v), want = %v", tl.Name, hasProp, hasRequired, want)
				}
			}
		})
	}
}
