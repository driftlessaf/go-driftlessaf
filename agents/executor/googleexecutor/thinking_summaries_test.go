/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/google/go-cmp/cmp"
)

// sentThinkingConfig is the thinking config of a request actually sent.
type sentThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts"`
	ThinkingLevel   string `json:"thinkingLevel"`
	ThinkingBudget  *int32 `json:"thinkingBudget"`
}

// TestThinkingSummaries checks whether the request actually sent asks for
// thinking summaries: the model's provider facts decide by default, and
// WithThinkingSummaries overrides them either way. The option changes only
// the summaries, never the thinking depth, and adds no thinking config when
// neither WithEffort nor WithThinking sets one.
func TestThinkingSummaries(t *testing.T) {
	type opt = googleexecutor.Option[errCapRequest, errCapResponse]
	low := googleexecutor.WithEffort[errCapRequest, errCapResponse](effort.Low)
	summaries := func(enable bool) opt {
		return googleexecutor.WithThinkingSummaries[errCapRequest, errCapResponse](enable)
	}
	tests := []struct {
		name  string
		model string
		opts  []opt
		want  *sentThinkingConfig
	}{{
		name:  "gemini 4 leaves them out",
		model: "gemini-4-neon",
		opts:  []opt{low},
		want:  &sentThinkingConfig{ThinkingLevel: "LOW"},
	}, {
		name:  "gemini 4 with WithThinkingSummaries(true) asks for them",
		model: "gemini-4-neon",
		opts:  []opt{summaries(true), low},
		want:  &sentThinkingConfig{IncludeThoughts: true, ThinkingLevel: "LOW"},
	}, {
		name:  "gemini 3 asks for them",
		model: "gemini-3.8-flash",
		opts:  []opt{low},
		want:  &sentThinkingConfig{IncludeThoughts: true, ThinkingLevel: "LOW"},
	}, {
		name:  "gemini 3 with WithThinkingSummaries(false) leaves them out",
		model: "gemini-3.8-flash",
		opts:  []opt{low, summaries(false)},
		want:  &sentThinkingConfig{ThinkingLevel: "LOW"},
	}, {
		name:  "a thinking budget asks for them",
		model: "gemini-2.5-flash",
		opts:  []opt{googleexecutor.WithThinking[errCapRequest, errCapResponse](2048)},
		want:  &sentThinkingConfig{IncludeThoughts: true, ThinkingBudget: new(int32(2048))},
	}, {
		name:  "a thinking budget with WithThinkingSummaries(false) leaves them out",
		model: "gemini-2.5-flash",
		opts:  []opt{googleexecutor.WithThinking[errCapRequest, errCapResponse](2048), summaries(false)},
		want:  &sentThinkingConfig{ThinkingBudget: new(int32(2048))},
	}, {
		name:  "no thinking config without WithEffort or WithThinking",
		model: "gemini-4-neon",
		opts:  []opt{summaries(true)},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
				_, _ = fmt.Fprint(w, `{
					"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_submit","name":"submit_result","args":{"result":{"answer":"done"},"reasoning":"complete"}}}]},"finishReason":"STOP"}],
					"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
				}`)
			}))
			t.Cleanup(srv.Close)

			prompt, err := promptbuilder.NewPrompt("go")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			opts := append([]opt{
				googleexecutor.WithModel[errCapRequest, errCapResponse](tt.model),
				googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
				googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
				// Keep the reasoning parameter on every model, so one canned
				// submit fits every row.
				googleexecutor.WithToolReasoning[errCapRequest, errCapResponse](true),
				googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.GoogleToolForResponse[errCapResponse]),
			}, tt.opts...)
			exec, err := googleexecutor.New[errCapRequest, errCapResponse](newTestClient(t, srv.URL), prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			resp, err := exec.Execute(t.Context(), errCapRequest{}, map[string]googletool.Metadata[errCapResponse]{})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got, want := resp.Answer, "done"; got != want {
				t.Errorf("resp.Answer: got = %q, want = %q", got, want)
			}

			mu.Lock()
			defer mu.Unlock()
			var body struct {
				GenerationConfig struct {
					ThinkingConfig *sentThinkingConfig `json:"thinkingConfig"`
				} `json:"generationConfig"`
			}
			if err := json.Unmarshal(first, &body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if diff := cmp.Diff(tt.want, body.GenerationConfig.ThinkingConfig); diff != "" {
				t.Errorf("thinking config (-want, +got):\n%s", diff)
			}
		})
	}
}
