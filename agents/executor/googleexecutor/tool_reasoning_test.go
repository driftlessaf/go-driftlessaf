/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
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
		opts          []googleexecutor.Option[errCapRequest, errCapResponse]
		wantReasoning bool
	}{{
		name:          "gemini 3 keeps it",
		model:         "gemini-3.8-flash",
		wantReasoning: true,
	}, {
		name:  "gemini 4 omits it",
		model: "gemini-4-neon",
	}, {
		name:          "gemini 4 with WithToolReasoning(true) keeps it",
		model:         "gemini-4-neon",
		opts:          []googleexecutor.Option[errCapRequest, errCapResponse]{googleexecutor.WithToolReasoning[errCapRequest, errCapResponse](true)},
		wantReasoning: true,
	}, {
		name:  "gemini 3 with WithToolReasoning(false) omits it",
		model: "gemini-3.8-flash",
		opts:  []googleexecutor.Option[errCapRequest, errCapResponse]{googleexecutor.WithToolReasoning[errCapRequest, errCapResponse](false)},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]any{"result": map[string]any{"answer": "done"}}
			if tt.wantReasoning {
				args["reasoning"] = "complete"
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var first []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				if first == nil {
					first = body
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{
					"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_submit","name":"submit_result","args":%s}}]},"finishReason":"STOP"}],
					"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
				}`, argsJSON)
			}))
			t.Cleanup(srv.Close)

			prompt, err := promptbuilder.NewPrompt("go")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			opts := append([]googleexecutor.Option[errCapRequest, errCapResponse]{
				googleexecutor.WithModel[errCapRequest, errCapResponse](tt.model),
				googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
				googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
				googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.GoogleToolForResponse[errCapResponse]),
			}, tt.opts...)
			exec, err := googleexecutor.New[errCapRequest, errCapResponse](newTestClient(t, srv.URL), prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			handler := func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": true}
			}
			tools := map[string]googletool.Metadata[errCapResponse]{
				"lookup": googletool.FromTool(toolcall.Tool[errCapResponse]{
					Def:     toolcall.Definition{Name: "lookup", Description: "lookup"},
					Handler: handler,
				}),
				"handmade": {
					Definition: &genai.FunctionDeclaration{
						Name: "handmade",
						Parameters: &genai.Schema{
							Type:       genai.TypeObject,
							Properties: map[string]*genai.Schema{"reasoning": {Type: genai.TypeString}},
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
					FunctionDeclarations []struct {
						Name       string `json:"name"`
						Parameters struct {
							Properties map[string]any `json:"properties"`
							Required   []string       `json:"required"`
						} `json:"parameters"`
					} `json:"functionDeclarations"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(first, &body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			var seen []string
			for _, tool := range body.Tools {
				for _, decl := range tool.FunctionDeclarations {
					seen = append(seen, decl.Name)
					want := tt.wantReasoning || decl.Name == "handmade"
					_, hasProp := decl.Parameters.Properties["reasoning"]
					hasRequired := slices.Contains(decl.Parameters.Required, "reasoning")
					if hasProp != want || hasRequired != want {
						t.Errorf("tool %q reasoning: got = (property %v, required %v), want = %v", decl.Name, hasProp, hasRequired, want)
					}
				}
			}
			slices.Sort(seen)
			if got, want := seen, []string{"handmade", "lookup", "submit_result"}; !slices.Equal(got, want) {
				t.Errorf("advertised tools: got = %v, want = %v", got, want)
			}
		})
	}
}
