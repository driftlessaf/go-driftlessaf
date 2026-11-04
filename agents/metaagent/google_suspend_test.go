/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

// TestGeminiAgentParksAndResumesWithResults drives a routed Gemini agent with
// a suspend tool through a scripted generateContent server: the model parks,
// ResumeWithResults sends the caller's reply as the park call's response, and
// the resumed conversation submits.
func TestGeminiAgentParksAndResumesWithResults(t *testing.T) {
	responses := []string{
		`{"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"park","args":{"question":"which branch?"}},"thoughtSignature":"c2ln"}
		]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
		`{"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"submit_result","args":{"reasoning":"done","result":{"answer":"main"}}}}
		]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
	}
	var mu sync.Mutex
	var bodies [][]byte
	var cacheBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "cachedContents") {
			mu.Lock()
			cacheBody = body
			mu.Unlock()
			_, _ = io.WriteString(w, `{"name":"cachedContents/c","expireTime":"2099-01-01T00:00:00Z"}`)
			return
		}
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, body)
		mu.Unlock()
		if n >= len(responses) {
			t.Errorf("unexpected request %d", n+1)
			n = len(responses) - 1
		}
		_, _ = io.WriteString(w, responses[n])
	}))
	t.Cleanup(srv.Close)

	selection := modelrouter.Selection{Provider: "scripted", LogicalModel: "gemini-2.5-flash"}
	adapters, err := NewGoogleGenAIAdapterRegistry(GoogleGenAIRegistration{
		Provider: selection.Provider,
		Adapter: func(ctx context.Context, plan modelrouter.Plan) (GoogleGenAIBinding, error) {
			client, err := genai.NewClient(ctx, &genai.ClientConfig{
				Backend:     genai.BackendGeminiAPI,
				APIKey:      "scripted",
				HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL},
			})
			if err != nil {
				return GoogleGenAIBinding{}, err
			}
			return NewGoogleGenAIBinding(plan, client, nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, routedTestRoute(selection, modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash")),
		AdapterRegistries{GoogleGenAI: adapters})
	cfg := routedTestConfig(t)
	cfg.SuspendToolName, cfg.SuspendToolDescription = "park", "Park the run until the operator replies."
	newAgent := func() ResultResumer[*testRequest, *basetenTestResponse, toolcall.EmptyTools] {
		agent, err := NewRouted[*testRequest](t.Context(), router, selection, cfg)
		if err != nil {
			t.Fatalf("NewRouted: %v", err)
		}
		rr, ok := AsResultResumer(agent)
		if !ok {
			t.Fatal("AsResultResumer() = false for a Gemini-backed agent")
		}
		return rr
	}

	ctx := agenttrace.WithTracer[*basetenTestResponse](t.Context(), new(basetenRecordingTracer))
	_, err = newAgent().Execute(ctx, &testRequest{}, toolcall.EmptyTools{})
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got %v, want a suspension", err)
	}
	if got, want := susp.Question, "which branch?"; got != want {
		t.Errorf("Question: got = %q, want = %q", got, want)
	}
	if len(susp.PendingToolCalls) != 1 {
		t.Fatalf("PendingToolCalls: got %+v, want the park call", susp.PendingToolCalls)
	}
	id := susp.PendingToolCalls[0].ID

	const reply = "use main"
	resp, err := newAgent().ResumeWithResults(ctx, susp.Envelope, map[string]string{id: reply}, toolcall.EmptyTools{})
	if err != nil {
		t.Fatalf("ResumeWithResults: %v", err)
	}
	if resp == nil || resp.Answer != "main" {
		t.Errorf("resumed Resp: got %+v, want the submission", resp)
	}

	mu.Lock()
	defer mu.Unlock()
	// The tool declarations travel in the context cache the run creates.
	var cached struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Name       string `json:"name"`
				Parameters struct {
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(cacheBody, &cached); err != nil {
		t.Fatal(err)
	}
	var parkRequired []string
	for _, tool := range cached.Tools {
		for _, d := range tool.FunctionDeclarations {
			if d.Name == "park" {
				parkRequired = d.Parameters.Required
			}
		}
	}
	if diff := cmp.Diff([]string{suspendQuestionProperty}, parkRequired); diff != "" {
		t.Errorf("park tool required properties (-want +got):\n%s", diff)
	}

	var resumed struct {
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(bodies[1], &resumed); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{
		"name": "park", "response": map[string]any{"output": reply},
	}}}}
	if diff := cmp.Diff(want, resumed.Contents[len(resumed.Contents)-1]); diff != "" {
		t.Errorf("resumed park response (-want +got):\n%s", diff)
	}
}
