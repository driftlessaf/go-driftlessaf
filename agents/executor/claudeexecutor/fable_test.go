/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
)

// Drive the real streaming SDK and execution loop. The server checks request
// shape, not model intelligence or live provider access. Fable permits
// cache_control relocation but requires the rest of the transcript, including
// empty thinking blocks and opaque signatures, to remain unchanged.
func TestFableTerminalSubmissionAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		finalize    int
		noSubmit    bool
	}{
		{name: "Fable redirect", model: "claude-fable-5-1"},
		{name: "Fable early finalize", model: "claude-fable-5-1", finalize: 1},
		{name: "Fable never submits", model: "claude-fable-5-1", noSubmit: true},
		{name: "existing Claude still forces redirect", model: "claude-sonnet-5", finalize: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var previous map[string]any
			requests, lookups, validations := 0, 0, 0
			srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
				requests = n
				var current map[string]any
				if err := json.Unmarshal(body, &current); err != nil {
					t.Errorf("request JSON: %v", err)
					return nil
				}
				if current["model"] != "opaque-provider-deployment" {
					t.Errorf("wire model = %v, want routed deployment", current["model"])
				}
				choice, _ := current["tool_choice"].(map[string]any)
				if tc.model == "claude-fable-5-1" {
					if choice != nil && choice["type"] != "auto" {
						t.Errorf("Fable request %d forces tool choice: %v", n, choice)
					}
				} else if n == 2 || n == 3 {
					if choice["type"] != "tool" || choice["name"] != "submit_result" {
						t.Errorf("existing Claude request %d no longer forces submission: %v", n, choice)
					}
				}
				for _, key := range []string{"temperature", "top_p", "top_k"} {
					if _, ok := current[key]; ok {
						t.Errorf("unsupported %s in adaptive-thinking request", key)
					}
				}
				if thinking, ok := current["thinking"].(map[string]any); ok && thinking["type"] != "adaptive" {
					t.Errorf("thinking = %v, want omitted or adaptive", thinking)
				}
				stripCacheControl(current)
				messages := current["messages"].([]any)
				if previous != nil {
					prefix := previous["messages"].([]any)
					if len(messages) <= len(prefix) {
						t.Errorf("request %d did not append history", n)
					} else if diff := cmp.Diff(prefix, messages[:len(prefix)]); diff != "" {
						t.Errorf("history changed (-want, +got): %s", diff)
					}
					for _, key := range []string{"system", "tools"} {
						if diff := cmp.Diff(previous[key], current[key]); diff != "" {
							t.Errorf("%s changed (-want, +got): %s", key, diff)
						}
					}
					assistant := messages[1].(map[string]any)["content"].([]any)
					want := map[string]any{"type": "thinking", "thinking": "", "signature": "opaque-signature-1"}
					if diff := cmp.Diff(want, assistant[0]); diff != "" {
						t.Errorf("opaque thinking replay (-want, +got): %s", diff)
					}
				}
				previous = current
				switch {
				case n == 1:
					return fableTestTurn(t, n, "lookup", map[string]any{"reasoning": "inspect"})
				case n == 2 || tc.noSubmit:
					return fableTestTurn(t, n, "", nil)
				case n == 3:
					return fableTestTurn(t, n, "submit_result", submitInput("wrong"))
				default:
					return fableTestTurn(t, n, "submit_result", submitInput("correct"))
				}
			})
			prompt, err := promptbuilder.NewPrompt("Review this synthetic changeset.")
			if err != nil {
				t.Fatal(err)
			}
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client()), option.WithAPIKey("test"), option.WithMaxRetries(0))
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
				claudeexecutor.WithRoutedModel[errCapRequest, errCapResponse]("opaque-provider-deployment", tc.model),
				claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](4),
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithMaxToolCallsBeforeFinalize[errCapRequest, errCapResponse](tc.finalize),
				claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
				claudeexecutor.WithResultValidator[errCapRequest, errCapResponse](func(_ context.Context, result errCapResponse, _ string) ([]callbacks.Finding, error) {
					validations++
					if result.Answer != "correct" {
						return []callbacks.Finding{{Kind: callbacks.FindingKindReview, Identifier: "wrong-answer", Details: "Try again."}}, nil
					}
					return nil, nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{
				"lookup": {
					Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
					Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
						lookups++
						return map[string]any{"result": "synthetic evidence"}
					},
				},
			})
			if tc.noSubmit {
				if err == nil || !strings.Contains(err.Error(), "maximum conversation turns") || validations != 0 {
					t.Errorf("missing submission: error = %v, validations = %d, want turn-limit failure without validation", err, validations)
				}
			} else if err != nil || result.Answer != "correct" || validations != 2 {
				t.Errorf("Execute: result = %+v, error = %v, validations = %d, want validated correct result after rejection", result, err, validations)
			}
			if requests != 4 || lookups != 1 {
				t.Errorf("requests/lookups = %d/%d, want 4/1", requests, lookups)
			}
		})
	}
}

func stripCacheControl(value any) {
	switch value := value.(type) {
	case map[string]any:
		delete(value, "cache_control")
		for _, child := range value {
			stripCacheControl(child)
		}
	case []any:
		for _, child := range value {
			stripCacheControl(child)
		}
	}
}

func fableTestTurn(t *testing.T, n int, tool string, input map[string]any) []string {
	t.Helper()
	events := []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_%d","type":"message","role":"assistant","content":[],"model":"claude-fable-5-1","usage":{"input_tokens":10,"output_tokens":1}}}`, n),
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature-%d"}}`, n),
		`{"type":"content_block_stop","index":0}`,
	}
	stop := "end_turn"
	if tool == "" {
		events = append(events,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"{\"answer\":\"correct\"}"}}`)
	} else {
		stop = "tool_use"
		b, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		partial, err := json.Marshal(string(b))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events,
			fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_%d","name":%q,"input":{}}}`, n, tool),
			fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial))
	}
	return append(events, `{"type":"content_block_stop","index":1}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":5}}`, stop),
		`{"type":"message_stop"}`)
}
