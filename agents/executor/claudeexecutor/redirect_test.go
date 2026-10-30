/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// widgetResponse registers its terminal tool under a name other than the
// default, the way callers such as mentat's submit_genome do.
type widgetResponse struct {
	_      struct{} `submitresult:"name=submit_widget"`
	Answer string   `json:"answer"`
}

// TestTextTurnRedirectNamesConfiguredSubmitTool pins the redirect that follows
// a text-only turn: it must name the submit tool the model was actually given.
// Naming the default submit_result instead tells the model to call a tool that
// does not exist, which it then reasons about turn after turn.
func TestTextTurnRedirectNamesConfiguredSubmitTool(t *testing.T) {
	var redirect string
	srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
		if n == 1 {
			return fableTestTurn(t, n, "", nil)
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("request JSON: %v", err)
			return nil
		}
		last := req.Messages[len(req.Messages)-1]
		for _, c := range last.Content {
			if c.Type == "text" {
				redirect = c.Text
			}
		}
		return fableTestTurn(t, n, "submit_widget", submitInput("correct"))
	})
	prompt, err := promptbuilder.NewPrompt("Review this synthetic changeset.")
	if err != nil {
		t.Fatal(err)
	}
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client()), option.WithAPIKey("test"), option.WithMaxRetries(0))
	exec, err := claudeexecutor.New[errCapRequest, widgetResponse](client, prompt,
		claudeexecutor.WithModel[errCapRequest, widgetResponse]("claude-sonnet-5"),
		claudeexecutor.WithMaxTurns[errCapRequest, widgetResponse](3),
		claudeexecutor.WithRetryConfig[errCapRequest, widgetResponse](fastRetry(0)),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, widgetResponse](submitresult.ClaudeToolForResponse[widgetResponse]),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[widgetResponse]{})
	if err != nil || result.Answer != "correct" {
		t.Fatalf("Execute: result = %+v, error = %v, want the submitted answer", result, err)
	}
	if !strings.Contains(redirect, "call the submit_widget tool") {
		t.Errorf("redirect: got = %q, want it to name submit_widget", redirect)
	}
	if strings.Contains(redirect, "submit_result") {
		t.Errorf("redirect: got = %q, want no mention of the default submit_result", redirect)
	}
}

// TestTextTurnRedirectOnAutoOnlyModelAsksForToolCalls pins the redirect's
// two shapes. A model that accepts a forced tool_choice is told it must call
// the submit tool, and the next request forces it. An automatic-only model
// with working tools is forced nothing and told to do the remaining work
// with tool calls, calling the submit tool only when done: pushing it at
// the terminal tool would end an agent whose text was a plan, not a result.
// Without working tools the automatic-only model keeps the must-call text,
// since the submit tool is all it has; so does one whose next turn is its
// last, since work asked for then could not be followed by the submit call.
func TestTextTurnRedirectOnAutoOnlyModelAsksForToolCalls(t *testing.T) {
	for _, tc := range []struct {
		name, model  string
		workingTools bool
		maxTurns     int // 0 means 3: a turn to work in after the redirect's
		wantForced   bool
		wantText     string
	}{
		{name: "forced model with tools", model: "claude-sonnet-5", workingTools: true, wantForced: true, wantText: "You must call the submit_widget tool"},
		{name: "auto-only model with tools", model: "claude-mythos-5-1", workingTools: true, wantText: "do that work now with tool calls. Call the submit_widget tool only when the task is complete"},
		{name: "auto-only model without tools", model: "claude-fable-5-1", wantText: "You must call the submit_widget tool"},
		{name: "auto-only model with tools on its last turn", model: "claude-mythos-5-1", workingTools: true, maxTurns: 2, wantText: "You must call the submit_widget tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var redirect string
			var choice map[string]any
			srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
				if n == 1 {
					return fableTestTurn(t, n, "", nil)
				}
				var req struct {
					ToolChoice map[string]any `json:"tool_choice"`
					Messages   []struct {
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					t.Errorf("request JSON: %v", err)
					return nil
				}
				choice = req.ToolChoice
				for _, c := range req.Messages[len(req.Messages)-1].Content {
					if c.Type == "text" {
						redirect = c.Text
					}
				}
				return fableTestTurn(t, n, "submit_widget", submitInput("correct"))
			})
			prompt, err := promptbuilder.NewPrompt("Review this synthetic changeset.")
			if err != nil {
				t.Fatal(err)
			}
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client()), option.WithAPIKey("test"), option.WithMaxRetries(0))
			exec, err := claudeexecutor.New[errCapRequest, widgetResponse](client, prompt,
				claudeexecutor.WithModel[errCapRequest, widgetResponse](tc.model),
				claudeexecutor.WithMaxTurns[errCapRequest, widgetResponse](cmp.Or(tc.maxTurns, 3)),
				claudeexecutor.WithRetryConfig[errCapRequest, widgetResponse](fastRetry(0)),
				claudeexecutor.WithSubmitResultProvider[errCapRequest, widgetResponse](submitresult.ClaudeToolForResponse[widgetResponse]),
			)
			if err != nil {
				t.Fatal(err)
			}
			tools := map[string]claudetool.Metadata[widgetResponse]{}
			if tc.workingTools {
				tools["lookup"] = claudetool.Metadata[widgetResponse]{
					Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
					Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[widgetResponse], *widgetResponse) map[string]any {
						return map[string]any{"result": "synthetic evidence"}
					},
				}
			}
			result, err := exec.Execute(t.Context(), errCapRequest{}, tools)
			if err != nil || result.Answer != "correct" {
				t.Fatalf("Execute: result = %+v, error = %v, want the submitted answer", result, err)
			}
			if !strings.Contains(redirect, tc.wantText) {
				t.Errorf("redirect = %q, want it to contain %q", redirect, tc.wantText)
			}
			forced := choice != nil && choice["type"] == "tool"
			if forced != tc.wantForced {
				t.Errorf("tool_choice = %v, want forced = %v", choice, tc.wantForced)
			}
		})
	}
}
