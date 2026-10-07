/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"strings"
	"testing"

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
