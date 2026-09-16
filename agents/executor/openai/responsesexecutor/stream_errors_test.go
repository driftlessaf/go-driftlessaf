/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go"
)

func TestTerminalStreamDiagnostics(t *testing.T) {
	t.Parallel()
	const requestID = "12345678-1234-1234-1234-123456789abc"
	const responseID = "resp_0123456789abcdef0123456789abcdef"
	const openaiRequestID = "req_0123456789abcdef0123456789abcdef"
	const canary = "private-stream-canary"
	for _, tt := range []struct {
		name       string
		eventType  string
		code       string
		reason     string
		id         string
		requestID  string
		fallbackID string
		partial    bool
		wantFields string
	}{
		{name: "failed", eventType: "response.failed", code: "server_error", id: responseID, requestID: requestID,
			wantFields: "; code=server_error; request_id=" + requestID + "; response_id=" + responseID},
		{name: "failed after partial tool", eventType: "response.failed", code: "rate_limit_exceeded", partial: true,
			wantFields: "; code=rate_limit_exceeded; response_id=" + responseID},
		{name: "output limit", eventType: "response.incomplete", reason: "max_output_tokens", id: responseID,
			wantFields: "; incomplete_reason=max_output_tokens; response_id=" + responseID},
		{name: "legacy output limit", eventType: "response.incomplete", reason: "max_tokens",
			wantFields: "; incomplete_reason=max_tokens"},
		{name: "content filter", eventType: "response.incomplete", reason: "content_filter", partial: true,
			wantFields: "; incomplete_reason=content_filter; response_id=" + responseID},
		{name: "bare error", eventType: "error", code: "invalid_request_error", requestID: requestID,
			wantFields: "; code=invalid_request_error; request_id=" + requestID},
		{name: "bare error after partial tool", eventType: "error", code: "rate_limit_exceeded", partial: true,
			wantFields: "; code=rate_limit_exceeded; response_id=" + responseID},
		{name: "OpenAI header fallback", eventType: "response.failed", requestID: canary, fallbackID: openaiRequestID,
			wantFields: "; request_id=" + openaiRequestID},
		{name: "terminal identity takes precedence", eventType: "response.failed", partial: true, id: requestID,
			wantFields: "; response_id=" + requestID},
		{name: "unknown fields", eventType: "response.incomplete", code: canary, reason: canary, id: canary, requestID: canary},
		{name: "unknown bare code", eventType: "error", code: canary},
		{name: "missing details", eventType: "response.failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Amzn-Requestid", tt.requestID)
				w.Header().Set("X-Request-Id", tt.fallbackID)
				w.Header().Set("X-Private-Header", canary)
				send := func(eventType string, event map[string]any) {
					event["type"] = eventType
					b, err := json.Marshal(event)
					if err != nil {
						t.Error(err)
						return
					}
					if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b); err != nil {
						t.Error(err)
					}
				}
				if tt.partial {
					send("response.created", map[string]any{"response": map[string]any{"id": responseID, "status": "in_progress"}})
					send("response.output_item.added", map[string]any{"item": call("read", "read_fixture", `{}`)})
					send("response.function_call_arguments.delta", map[string]any{"delta": canary})
				}
				if tt.eventType == "error" {
					send(tt.eventType, map[string]any{"code": tt.code, "message": canary, "param": canary})
					return
				}
				send(tt.eventType, map[string]any{"response": map[string]any{
					"id": tt.id, "status": strings.TrimPrefix(tt.eventType, "response."),
					"error":              map[string]any{"code": tt.code, "message": canary},
					"incomplete_details": map[string]any{"reason": tt.reason},
					"output":             []any{call("read", "read_fixture", `{}`), submit("submit", canary)},
				}})
			}, config(t))
			tracer := new(recordingTracer)
			got, err := e.Execute(agenttrace.WithTracer[answer](t.Context(), tracer), request{}, map[string]toolcall.Tool[answer]{
				"read_fixture": {Def: toolcall.Definition{Name: "read_fixture"}, Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[answer], *answer) map[string]any {
					t.Error("tool called for an unsuccessful stream")
					return nil
				}},
			})
			want := "responses stream failed or ended incomplete; event=" + tt.eventType + tt.wantFields + "; usage may be unavailable"
			if err == nil || err.Error() != want {
				t.Fatalf("error: got = %v, want = %q", err, want)
			}
			if retryable(err) || statusCode(err) != -1 || attempts.Load() != 1 {
				t.Errorf("retryable/status/attempts: got = %t/%d/%d, want = false/-1/1", retryable(err), statusCode(err), attempts.Load())
			}
			if _, ok := errors.AsType[*openai.Error](err); ok {
				t.Error("error retains the raw SDK error")
			}
			if got != (answer{}) {
				t.Errorf("result: got = %+v, want zero value", got)
			}
			if tracer.trace == nil || len(tracer.trace.Turns) != 1 || tracer.trace.Error == nil {
				t.Fatal("missing failed execution trace")
			}
			if diff := cmp.Diff([]string{want}, tracer.trace.Turns[0].Errors); diff != "" {
				t.Errorf("turn errors (-want, +got):\n%s", diff)
			}
			if tracer.trace.Error.Error() != want || !tracer.trace.Turns[0].Failed {
				t.Errorf("trace failure: got = %v/%t, want = %q/true", tracer.trace.Error, tracer.trace.Turns[0].Failed, want)
			}
			b, err := json.Marshal(tracer.trace)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), canary) {
				t.Error("trace leaked untrusted stream content")
			}
		})
	}
}

func TestSafeResponseID(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		id   string
		want string
	}{
		{id: "12345678-1234-1234-1234-123456789abc", want: "12345678-1234-1234-1234-123456789abc"},
		{id: "resp_" + strings.Repeat("a", 32), want: "resp_" + strings.Repeat("a", 32)},
		{id: "resp_" + strings.Repeat("A", 48), want: "resp_" + strings.Repeat("A", 48)},
		{id: "resp_" + strings.Repeat("0", 64), want: "resp_" + strings.Repeat("0", 64)},
		{id: "req_" + strings.Repeat("a", 32)},
		{id: "resp_" + strings.Repeat("a", 31)},
		{id: "resp_" + strings.Repeat("a", 65)},
		{id: "resp_" + strings.Repeat("z", 32)},
		{id: "resp_" + strings.Repeat("a", 31) + "\n"},
		{id: "resp_private-canary"},
		{},
	} {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			if got := safeResponseID(tt.id); got != tt.want {
				t.Errorf("response ID: got = %q, want = %q", got, tt.want)
			}
		})
	}
}
