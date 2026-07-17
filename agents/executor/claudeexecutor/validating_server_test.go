/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// validatingServerMaxCacheBreakpoints mirrors the unexported
// claudeexecutor.maxCacheBreakpoints — the Anthropic API's hard limit on the
// number of cache_control markers a single request may carry. The validating
// server asserts every request the executor emits stays within it, so a
// breakpoint-budget regression (for example a resume path that re-seeds
// markers without clearing the old ones) surfaces as a test failure instead of
// a non-retryable 400 in production.
const validatingServerMaxCacheBreakpoints = 4

// validatingReqBody is the subset of the Anthropic Messages request the
// validating server inspects: the cache_control markers on the tool
// definitions and system blocks, and the message transcript with enough of
// each content block to check tool_use/tool_result pairing and count markers.
type validatingReqBody struct {
	System []struct {
		CacheControl *json.RawMessage `json:"cache_control"`
	} `json:"system"`
	Tools []struct {
		CacheControl *json.RawMessage `json:"cache_control"`
	} `json:"tools"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type         string           `json:"type"`
			ID           string           `json:"id"`
			ToolUseID    string           `json:"tool_use_id"`
			CacheControl *json.RawMessage `json:"cache_control"`
		} `json:"content"`
	} `json:"messages"`
}

// newValidatingAnthropicServer returns an httptest server that stands in for
// the Anthropic Messages streaming API. For each request it parses the body
// and, before streaming a scripted SSE response, asserts two invariants the
// real API enforces with non-retryable 400s:
//
//   - tool_use/tool_result pairing: every tool_use block in an assistant
//     message has a matching tool_result (keyed by tool_use_id) in the
//     immediately following user message. This is exactly the shape a verbatim
//     transcript replay (the DEV-2247 resume risk) would violate.
//   - cache breakpoint budget: the total number of cache_control markers across
//     the tool definitions, the system blocks, and every message content block
//     never exceeds validatingServerMaxCacheBreakpoints.
//
// script returns the SSE events (raw JSON, one per event) to stream for the
// given 1-based request number; body is the parsed-out raw request body so a
// script can vary its response on what the model was sent. Assertion failures
// are reported with t.Errorf (safe from the handler goroutine) so the driving
// test fails without aborting the in-flight HTTP exchange.
func newValidatingAnthropicServer(t *testing.T, script func(reqNum int, body []byte) []string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var reqNum int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		reqNum++
		n := reqNum
		mu.Unlock()

		assertRequestValid(t, n, body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, script(n, body)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertRequestValid reports every pairing and cache-budget violation in a
// single request body as a test error.
func assertRequestValid(t *testing.T, reqNum int, body []byte) {
	t.Helper()
	for _, v := range requestViolations(body) {
		t.Errorf("request %d: %s", reqNum, v)
	}
}

// requestViolations parses an Anthropic Messages request body and returns a
// message for every violation of the two invariants the validating server
// enforces (tool_use/tool_result pairing and the cache-breakpoint budget).
// A nil return means the body is well-formed; a parse error is reported as a
// single violation. Returning violations (rather than asserting inline) lets
// the Rejects tests below prove the guard actually fires on broken bodies.
func requestViolations(body []byte) []string {
	var parsed validatingReqBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return []string{fmt.Sprintf("invalid JSON body: %v", err)}
	}

	var violations []string

	// Count cache_control markers across tools, system, and messages.
	markers := 0
	for _, tl := range parsed.Tools {
		if tl.CacheControl != nil {
			markers++
		}
	}
	for _, sys := range parsed.System {
		if sys.CacheControl != nil {
			markers++
		}
	}
	for _, m := range parsed.Messages {
		for _, cb := range m.Content {
			if cb.CacheControl != nil {
				markers++
			}
		}
	}
	if markers > validatingServerMaxCacheBreakpoints {
		violations = append(violations, fmt.Sprintf(
			"%d cache_control markers exceeds the API limit of %d",
			markers, validatingServerMaxCacheBreakpoints))
	}

	// Every tool_use block in an assistant message must be answered by a
	// matching tool_result in the immediately following user message.
	for i, m := range parsed.Messages {
		if m.Role != "assistant" {
			continue
		}
		var toolUseIDs []string
		for _, cb := range m.Content {
			if cb.Type == "tool_use" {
				toolUseIDs = append(toolUseIDs, cb.ID)
			}
		}
		if len(toolUseIDs) == 0 {
			continue
		}
		if i+1 >= len(parsed.Messages) {
			violations = append(violations, fmt.Sprintf(
				"assistant message %d carries tool_use blocks but no following user message answers them", i))
			continue
		}
		next := parsed.Messages[i+1]
		if next.Role != "user" {
			violations = append(violations, fmt.Sprintf(
				"message %d following an assistant tool_use turn has role %q, want user", i+1, next.Role))
		}
		results := make(map[string]struct{})
		for _, cb := range next.Content {
			if cb.Type == "tool_result" {
				results[cb.ToolUseID] = struct{}{}
			}
		}
		for _, id := range toolUseIDs {
			if _, ok := results[id]; !ok {
				violations = append(violations, fmt.Sprintf(
					"tool_use %q in assistant message %d has no matching tool_result in the following user message",
					id, i))
			}
		}
	}
	return violations
}

// TestRequestViolationsRejectsUnpairedTranscript proves the pairing check
// actually fires: each case is one way a replayed transcript (the DEV-2247
// resume risk) can break tool_use/tool_result pairing, and each must be
// flagged. Without these the validating server would be a blind mock that
// never catches a broken transcript.
func TestRequestViolationsRejectsUnpairedTranscript(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{{
		name: "tool_use with no following message",
		body: `{"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1"}]}]}`,
	}, {
		name: "message following tool_use is not user",
		body: `{"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1"}]},
			{"role":"assistant","content":[{"type":"text"}]}]}`,
	}, {
		name: "tool_result id does not match",
		body: `{"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_other"}]}]}`,
	}, {
		name: "one of two sibling tool_use calls unanswered",
		body: `{"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1"},{"type":"tool_use","id":"toolu_2"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1"}]}]}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestViolations([]byte(tc.body)); len(got) == 0 {
				t.Error("requestViolations accepted an unpaired transcript")
			}
		})
	}
}

// TestRequestViolationsCacheBudget proves the cache-breakpoint tally fires
// when markers across tools, system, and messages exceed the API budget, and
// stays quiet for an in-budget, well-paired body.
func TestRequestViolationsCacheBudget(t *testing.T) {
	overBudget := `{
		"system":[{"cache_control":{"type":"ephemeral"}}],
		"tools":[{"cache_control":{"type":"ephemeral"}},{"cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","cache_control":{"type":"ephemeral"}},
			{"type":"text","cache_control":{"type":"ephemeral"}}]}]}`
	if got := requestViolations([]byte(overBudget)); len(got) == 0 {
		t.Errorf("requestViolations accepted 5 cache_control markers, over the budget of %d",
			validatingServerMaxCacheBreakpoints)
	}

	wellFormed := `{
		"tools":[{"cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","cache_control":{"type":"ephemeral"}}]}]}`
	if got := requestViolations([]byte(wellFormed)); len(got) != 0 {
		t.Errorf("requestViolations flagged a well-formed in-budget body: %v", got)
	}
}

// TestValidatingAnthropicServerAcceptsPairedTranscript proves the validating
// server helper works end-to-end: it drives a two-turn conversation (a tool
// call, then a final text answer) so the second request replays a well-formed
// assistant(tool_use) → user(tool_result) pair, and the run completes without
// the server flagging a pairing or cache-budget violation.
func TestValidatingAnthropicServerAcceptsPairedTranscript(t *testing.T) {
	toolCallTurn := []string{
		`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_lookup","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"reasoning\":\"check\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
	finalTurn := []string{
		`{"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":20,"output_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"answer\":\"42\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}

	srv := newValidatingAnthropicServer(t, func(reqNum int, _ []byte) []string {
		if reqNum == 1 {
			return toolCallTurn
		}
		return finalTurn
	})

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{
				Name:        "lookup",
				Description: anthropic.String("Look something up."),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type:       "object",
					Properties: map[string]any{"reasoning": map[string]any{"type": "string"}},
				},
			},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"result": "ok"}
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
}
