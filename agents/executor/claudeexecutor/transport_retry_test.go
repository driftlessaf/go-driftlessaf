/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestTransportDropIsRetriedWithinTheTurn drives the executor against a fake
// API that streams message_start and then aborts the connection on the first
// request — the shape of a mid-thinking drop — and answers in full on the
// second. The turn must complete on the re-send, the API must have seen two
// requests, and the drop must land on the turn's Errors list.
func TestTransportDropIsRetriedWithinTheTurn(t *testing.T) {
	answer := []string{
		`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":1234,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"answer\":\"42\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// message_start reaches the client, then the connection is torn
			// down mid-body. On HTTP/1.1 the client reads an unexpected EOF.
			_, _ = io.WriteString(w, sseBody(t, answer[:1]))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = io.WriteString(w, sseBody(t, answer))
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0), // The executor's retry is the one under test.
	)
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(2)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracer := &recordingTracer{}
	resp, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v, want the turn to complete on the re-send", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("Answer: got = %q, want = %q", got, want)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests: got = %d, want = 2 (one drop, one answer)", got)
	}
	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 1 {
		t.Fatalf("recorded traces/turns: got = %d, want one trace with one turn", len(tracer.traces))
	}
	turn := tracer.traces[0].Turns[0]
	if turn.Failed {
		t.Error("turn.Failed: got = true, want = false after a recovered drop")
	}
	if len(turn.Errors) != 1 {
		t.Errorf("turn.Errors: got = %d, want = 1 (the dropped attempt, recorded by OnAttemptError)", len(turn.Errors))
	}
}

// TestTransportDropExhaustionFailsOnTheCountedPath drives the executor against
// a fake API that drops every request after message_start. Once the retries
// are spent the run must fail as INFRASTRUCTURE with NO requeue marker: a
// Delay requeue resets the dispatcher's attempt count, and a drop that repeats
// for one request has to stay on the path that dead-letters at maxRetry.
func TestTransportDropExhaustionFailsOnTheCountedPath(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, []string{
			`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":1234,"output_tokens":1}}}`,
		}))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	const maxRetries = 2
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(maxRetries)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracer := &recordingTracer{}
	_, execErr := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil)
	if execErr == nil {
		t.Fatal("Execute: got nil error, want the exhausted drop")
	}
	if got, want := requests.Load(), int32(maxRetries+1); got != want {
		t.Errorf("requests: got = %d, want = %d (every attempt was sent)", got, want)
	}
	if _, _, ok := workqueue.GetRequeueOptions(execErr); ok {
		t.Errorf("GetRequeueOptions(execErr) ok = true, want no requeue marker so the dispatcher keeps the attempt count: %v", execErr)
	}
	if !workqueue.HasInfrastructureMarker(execErr) {
		t.Errorf("HasInfrastructureMarker(execErr) = false, want = true: %v", execErr)
	}
	for _, want := range []string{"failed to stream Claude response", "EOF"} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("error text does not name %q: %v", want, execErr)
		}
	}
	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 1 {
		t.Fatalf("recorded traces/turns: got = %d, want one trace with one turn", len(tracer.traces))
	}
	turn := tracer.traces[0].Turns[0]
	if !turn.Failed {
		t.Error("turn.Failed: got = false, want = true after every attempt dropped")
	}
	if got, want := len(turn.Errors), maxRetries+1; got < want {
		t.Errorf("turn.Errors: got = %d, want >= %d (each dropped attempt, then the terminal error)", got, want)
	}
}
