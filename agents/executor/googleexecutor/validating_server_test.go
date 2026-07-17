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
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// validatingGenReqBody is the subset of the generateContent request the
// validating server inspects: the conversation contents with enough of each
// part to check functionCall/functionResponse pairing.
type validatingGenReqBody struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			FunctionCall *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"contents"`
}

// pairingKey identifies a functionCall or functionResponse for pairing: the
// provider-assigned id when present, falling back to the tool name. Gemini
// does not always populate functionCall ids, but when it does the executor
// echoes them onto the functionResponse, so ids give per-call granularity
// (two parallel calls to one tool pair independently) while the name fallback
// keeps id-less transcripts checkable.
func pairingKey(id, name string) string {
	if id != "" {
		return id
	}
	return name
}

// genPairingViolations parses a generateContent request body and returns a
// message for every functionCall/functionResponse pairing violation: every
// functionCall in a role:"model" content must be answered by a
// functionResponse with the matching name in the role:"user" content that
// immediately follows. Gemini bundles a turn's function responses into a
// single following user content. This is the shape a verbatim transcript
// replay (the DEV-2247 resume risk) would violate. A nil return means the body
// is well-paired; a parse error is reported as a single violation.
func genPairingViolations(body []byte) []string {
	var parsed validatingGenReqBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return []string{fmt.Sprintf("invalid JSON body: %v", err)}
	}

	var violations []string
	for i, c := range parsed.Contents {
		if c.Role != "model" {
			continue
		}
		type pendingCall struct{ id, name string }
		var calls []pendingCall
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				calls = append(calls, pendingCall{p.FunctionCall.ID, p.FunctionCall.Name})
			}
		}
		if len(calls) == 0 {
			continue
		}
		if i+1 >= len(parsed.Contents) {
			violations = append(violations, fmt.Sprintf(
				"model content %d carries functionCall parts but no following content answers them", i))
			continue
		}
		next := parsed.Contents[i+1]
		if next.Role != "user" {
			violations = append(violations, fmt.Sprintf(
				"content %d following a model functionCall turn has role %q, want user", i+1, next.Role))
		}
		// Responses are consumed as a multiset so two parallel calls to the
		// same tool each need their own functionResponse — a single response
		// cannot answer both.
		answered := make(map[string]int, len(next.Parts))
		for _, p := range next.Parts {
			if p.FunctionResponse != nil {
				answered[pairingKey(p.FunctionResponse.ID, p.FunctionResponse.Name)]++
			}
		}
		for _, call := range calls {
			key := pairingKey(call.id, call.name)
			if answered[key] == 0 {
				violations = append(violations, fmt.Sprintf(
					"functionCall %q in model content %d has no matching functionResponse in the following user content",
					call.name, i))
				continue
			}
			answered[key]--
		}
	}
	return violations
}

// cachedContentReply is the CachedContent shape the fake cache endpoint
// returns. expireTime is fixed far in the future so getOrCreateCache treats it
// as valid; the name is what resume-side context-cache re-creation (PR 8)
// asserts against. usageMetadata.totalTokenCount drives the cache-write metric.
const cachedContentReply = `{"name":"cachedContents/test-cache","model":"models/gemini-2.5-flash","expireTime":"2099-01-01T00:00:00Z","usageMetadata":{"totalTokenCount":5}}`

// newValidatingGenerateContentServer returns an httptest server that stands in
// for the Gemini generateContent API. For each generateContent request it
// parses the body and asserts functionCall/functionResponse pairing (see
// genPairingViolations) before returning a scripted response. It also serves a
// fake cachedContents create endpoint so tests can run with context caching
// enabled without reaching a real backend — the seam PR 8's context-cache
// re-creation tests build on. cacheReqs, when non-nil, counts each cache
// create call.
//
// script returns the generateContent response JSON for the given 1-based
// request number; body is the raw request body so a script can vary its
// response on what the model was sent. Assertion failures are reported with
// t.Errorf (safe from the handler goroutine) so the driving test fails without
// aborting the in-flight HTTP exchange.
func newValidatingGenerateContentServer(t *testing.T, cacheReqs *int, script func(reqNum int, body []byte) string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var reqNum int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// The cachedContents create call is a POST whose path ends in
		// /cachedContents (no :generateContent action suffix).
		if strings.Contains(r.URL.Path, "cachedContents") {
			mu.Lock()
			if cacheReqs != nil {
				*cacheReqs++
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, cachedContentReply)
			return
		}

		mu.Lock()
		reqNum++
		n := reqNum
		mu.Unlock()

		for _, v := range genPairingViolations(body) {
			t.Errorf("request %d: %s", n, v)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, script(n, body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// lookupTurnJSON is a generateContent response calling the lookup tool.
const lookupTurnJSON = `{
	"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_lookup","name":"lookup","args":{}}}]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

// submitTurnJSON is a generateContent response calling submit_result.
const submitTurnJSON = `{
	"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_submit","name":"submit_result","args":{"answer":"42"}}}]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

// TestValidatingGenerateContentServerAcceptsPairedTranscript proves the
// validating server helper works end-to-end with context caching enabled: it
// drives a lookup-tool turn followed by a submit turn, so the second request
// replays a well-formed model(functionCall) → user(functionResponse) pair, the
// fake cachedContents endpoint is exercised, and the run completes without the
// server flagging a pairing violation.
func TestValidatingGenerateContentServerAcceptsPairedTranscript(t *testing.T) {
	var cacheReqs int
	srv := newValidatingGenerateContentServer(t, &cacheReqs, func(reqNum int, _ []byte) string {
		if reqNum == 1 {
			return lookupTurnJSON
		}
		return submitTurnJSON
	})

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	sysPrompt, err := promptbuilder.NewPrompt("system")
	if err != nil {
		t.Fatalf("NewPrompt (system): %v", err)
	}

	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		// System instructions ensure getOrCreateCache runs, exercising the
		// fake cachedContents endpoint (cache-on is the default).
		googleexecutor.WithSystemInstructions[errCapRequest, errCapResponse](sysPrompt),
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (googletool.SubmitMetadata[errCapResponse], error) {
			return googletool.SubmitMetadata[errCapResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_result"},
				Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
					answer, _ := call.Args["answer"].(string)
					return toolcall.SubmitOutcome[errCapResponse]{
						Accepted:   true,
						Response:   errCapResponse{Answer: answer},
						ToolResult: map[string]any{"success": true},
					}
				},
			}, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := map[string]googletool.Metadata[errCapResponse]{
		"lookup": {
			Definition: &genai.FunctionDeclaration{Name: "lookup"},
			Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
				return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"ok": true}}
			},
		},
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if cacheReqs == 0 {
		t.Error("fake cachedContents endpoint was never called; context cache path not exercised")
	}
}

// TestGenPairingViolationsRejectsUnpairedTranscript proves the pairing check
// actually fires: a model functionCall with no following functionResponse must
// be flagged. Without this the validating server would be a blind mock that
// never catches a broken transcript.
func TestGenPairingViolationsRejectsUnpairedTranscript(t *testing.T) {
	unpaired := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"lookup"}}]}
	]}`)
	if got := genPairingViolations(unpaired); len(got) == 0 {
		t.Error("genPairingViolations accepted a model functionCall with no matching functionResponse")
	}
}

// TestGenPairingViolationsParallelSameNameCalls proves the pairing check has
// per-call granularity: two parallel calls to the same tool pair only when
// each has its own functionResponse — one response must not answer both.
func TestGenPairingViolationsParallelSameNameCalls(t *testing.T) {
	for _, tc := range []struct {
		name           string
		body           string
		wantViolations int
	}{{
		name: "both calls answered",
		body: `{"contents":[
			{"role":"model","parts":[
				{"functionCall":{"id":"call_1","name":"lookup"}},
				{"functionCall":{"id":"call_2","name":"lookup"}}]},
			{"role":"user","parts":[
				{"functionResponse":{"id":"call_1","name":"lookup"}},
				{"functionResponse":{"id":"call_2","name":"lookup"}}]}
		]}`,
		wantViolations: 0,
	}, {
		name: "second same-name call unanswered",
		body: `{"contents":[
			{"role":"model","parts":[
				{"functionCall":{"id":"call_1","name":"lookup"}},
				{"functionCall":{"id":"call_2","name":"lookup"}}]},
			{"role":"user","parts":[
				{"functionResponse":{"id":"call_1","name":"lookup"}}]}
		]}`,
		wantViolations: 1,
	}, {
		name: "id-less same-name calls fall back to name multiset",
		body: `{"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"lookup"}},
				{"functionCall":{"name":"lookup"}}]},
			{"role":"user","parts":[
				{"functionResponse":{"name":"lookup"}}]}
		]}`,
		wantViolations: 1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := genPairingViolations([]byte(tc.body))
			if len(got) != tc.wantViolations {
				t.Errorf("violations: got = %d (%v), want = %d", len(got), got, tc.wantViolations)
			}
		})
	}
}
