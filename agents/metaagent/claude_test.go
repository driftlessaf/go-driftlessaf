/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestClaudeTemperatureOptions(t *testing.T) {
	tests := []struct {
		name    string
		modelID string
		want    int
	}{
		{name: "sonnet 5 adaptive", modelID: "claude-sonnet-5", want: 0},
		{name: "sonnet 5 vertex version", modelID: "claude-sonnet-5@20260801", want: 0},
		{name: "sonnet 5 bedrock", modelID: "anthropic.claude-sonnet-5", want: 0},
		{name: "opus 4.8 adaptive", modelID: "claude-opus-4-8", want: 0},
		{name: "sonnet 4.6 sampling", modelID: "claude-sonnet-4-6", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(claudeTemperatureOptions[*testRequest, *testResponse](tt.modelID)); got != tt.want {
				t.Errorf("claudeTemperatureOptions(%q): got = %d, want = %d", tt.modelID, got, tt.want)
			}
		})
	}
}

func TestClaudeDisableTailPromptCaching(t *testing.T) {
	for _, tt := range []struct {
		name         string
		disable      bool
		wantMessages bool
	}{
		{name: "tail caching on by default", disable: false, wantMessages: true},
		{name: "tail caching disabled", disable: true, wantMessages: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte
			client := anthropic.NewClient(
				option.WithoutEnvironmentDefaults(),
				option.WithAPIKey("placeholder"),
				option.WithBaseURL("https://anthropic.invalid"),
				option.WithMaxRetries(0),
				option.WithHTTPClient(&http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					var err error
					body, err = io.ReadAll(io.LimitReader(request.Body, 1<<20))
					if err != nil {
						t.Errorf("reading request body: %v", err)
					}
					respBody := `{"type":"error","error":{"type":"authentication_error","message":"denied"}}`
					return &http.Response{
						StatusCode:    http.StatusUnauthorized,
						Status:        "401 Unauthorized",
						Header:        http.Header{"Content-Type": []string{"application/json"}},
						Body:          io.NopCloser(strings.NewReader(respBody)),
						ContentLength: int64(len(respBody)),
						Request:       request,
					}, nil
				})}),
			)

			config := routedClaudeTestConfig(t)
			system, err := promptbuilder.NewPrompt("shared rubric")
			if err != nil {
				t.Fatalf("NewPrompt (system): %v", err)
			}
			config.SystemInstructions = system
			config.DisableTailPromptCaching = tt.disable

			agent, err := newClaudeAgentWithMessages[*testRequest, *claudeAdapterTestResponse, toolcall.EmptyTools](claudeAgentConstruction{
				messages:        client.Messages,
				providerModelID: "claude-sonnet-4-6",
				logicalModelID:  "claude-sonnet-4-6",
			}, config)
			if err != nil {
				t.Fatalf("newClaudeAgentWithMessages: %v", err)
			}
			if _, err := agent.Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{}); err == nil {
				t.Fatal("Execute: got nil error, want authentication error")
			}
			if len(body) == 0 {
				t.Fatal("no request body captured")
			}

			type marked struct {
				CacheControl json.RawMessage `json:"cache_control"`
			}
			var req struct {
				System   []marked `json:"system"`
				Tools    []marked `json:"tools"`
				Messages []struct {
					Content []marked `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			if len(req.System) == 0 || len(req.Tools) == 0 || len(req.Messages) == 0 {
				t.Fatalf("request: got %d system blocks, %d tools and %d messages, want all non-empty", len(req.System), len(req.Tools), len(req.Messages))
			}
			var gotSystem, gotTools, gotMessages bool
			for _, s := range req.System {
				gotSystem = gotSystem || len(s.CacheControl) > 0
			}
			for _, tl := range req.Tools {
				gotTools = gotTools || len(tl.CacheControl) > 0
			}
			for _, m := range req.Messages {
				for _, c := range m.Content {
					gotMessages = gotMessages || len(c.CacheControl) > 0
				}
			}
			// The static prefix stays cached in both cases.
			if !gotSystem {
				t.Error("system cache_control marker: got = false, want = true")
			}
			if !gotTools {
				t.Error("tool cache_control marker: got = false, want = true")
			}
			if gotMessages != tt.wantMessages {
				t.Errorf("message cache_control marker: got = %t, want = %t", gotMessages, tt.wantMessages)
			}
		})
	}
}
