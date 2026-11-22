/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"chainguard.dev/driftlessaf/workqueue"
	"google.golang.org/genai"
)

// step is what the scripted server does with one generateContent request.
type step func(t *testing.T, w http.ResponseWriter)

// reply answers with body.
func reply(body string) step {
	return func(_ *testing.T, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// cutBody sends the start of a response and then aborts the connection, so
// the client reads an unexpected EOF.
func cutBody(_ *testing.T, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"candidates":[`)
	w.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

// resetConnection resets the TCP connection before any response byte, so
// the client reads ECONNRESET.
func resetConnection(t *testing.T, w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("Hijack: %v", err)
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// newScriptedServer answers the nth generateContent request with script's
// nth step and records each request body. A request past the end of script
// gets the last step. Context cache creates succeed and are not counted.
func newScriptedServer(t *testing.T, script ...step) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		if strings.Contains(r.URL.Path, "cachedContents") {
			reply(cachedContentReply)(t, w)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		script[min(n, len(script))-1](t, w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(bodies)
	}
}

// newDropTestExecutor builds an executor that retries a failed request once.
func newDropTestExecutor(t *testing.T, url string) googleexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, url),
		prompt,
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(1)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](2),
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
	return exec
}

// TestTransportDropIsRetriedWithinTheTurn checks that a request the
// transport dropped is sent again within the same turn, that the run
// completes, and that the drop lands on the turn's Errors list.
func TestTransportDropIsRetriedWithinTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop step
	}{
		{name: "body cut off mid-response", drop: cutBody},
		{name: "connection reset before the response", drop: resetConnection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, bodies := newScriptedServer(t, tc.drop, reply(submitTurnJSON))
			exec := newDropTestExecutor(t, srv.URL)

			tracer := &recordingTracer{}
			resp, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil)
			if err != nil {
				t.Fatalf("Execute: %v, want the turn to complete on the re-send", err)
			}
			if got, want := resp.Answer, "42"; got != want {
				t.Errorf("resp.Answer: got = %q, want = %q", got, want)
			}
			if got, want := len(bodies()), 2; got != want {
				t.Errorf("generateContent requests: got = %d, want = %d", got, want)
			}
			if got, want := len(tracer.traces), 1; got != want {
				t.Fatalf("recorded traces: got = %d, want = %d", got, want)
			}
			turns := tracer.traces[0].Turns
			if got, want := len(turns), 1; got != want {
				t.Fatalf("trace turns: got = %d, want = %d", got, want)
			}
			if got := len(turns[0].Errors); got != 1 {
				t.Errorf("turn errors: got = %d (%q), want = 1 (the dropped attempt)", got, turns[0].Errors)
			}
		})
	}
}

// TestTransportDropOnEveryAttempt checks that a request the transport drops
// on every attempt fails as infrastructure without a requeue delay, so the
// workqueue counts the attempt rather than resetting its count.
func TestTransportDropOnEveryAttempt(t *testing.T) {
	srv, bodies := newScriptedServer(t, cutBody)
	exec := newDropTestExecutor(t, srv.URL)

	_, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err == nil {
		t.Fatal("Execute: got nil error, want the drop to fail the run")
	}
	if !workqueue.IsInfrastructureError(err) {
		t.Errorf("Execute error %q: got no infrastructure marker, want one", err)
	}
	if delay, ok := workqueue.GetRequeueDelay(err); ok {
		t.Errorf("Execute error %q: got requeue delay %s, want none", err, delay)
	}
	if got, want := len(bodies()), 2; got != want {
		t.Errorf("generateContent requests: got = %d, want = %d (one attempt and one retry)", got, want)
	}
}

// TestTransportDropDuringLongDecode checks that a drop on a continuation
// request does not duplicate output. The failed generation leaves the chat
// history unchanged, so the retry sends the original request without a
// continuation token and the answer arrives once.
func TestTransportDropDuringLongDecode(t *testing.T) {
	const token = "Y29udGludWUtMQ=="
	head := reply(`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"answer\": \"4"}]},"finishReason":"CONTINUATION","continuationToken":"` + token + `"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":32768,"totalTokenCount":32778}}`)
	tail := reply(`{"candidates":[{"content":{"role":"model","parts":[{"text":"2\"}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`)
	srv, bodies := newScriptedServer(t, head, cutBody, head, tail)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithModel[errCapRequest, errCapResponse]("gemini-4-argon"),
		googleexecutor.WithMaxOutputTokens[errCapRequest, errCapResponse](200000),
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(1)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}

	got := bodies()
	if len(got) != 4 {
		t.Fatalf("generateContent requests: got = %d, want = 4 (head, dropped tail, head again, tail)", len(got))
	}
	for i, wantToken := range []bool{false, true, false, true} {
		if hasToken := strings.Contains(got[i], token); hasToken != wantToken {
			t.Errorf("request %d carries the continuation token: got = %v, want = %v", i+1, hasToken, wantToken)
		}
	}
	if got[0] != got[2] {
		t.Errorf("retried request differs from the original:\noriginal: %s\nretry:    %s", got[0], got[2])
	}
}
