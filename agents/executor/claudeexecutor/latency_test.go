/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

var (
	latencyToolCallTurn = []string{
		`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_lookup","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"reasoning\":\"check\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
	latencyFinalTurn = []string{
		`{"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":20,"output_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"answer\":\"42\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
)

// latencyNoDeltaTurn answers in one block carried whole by content_block_start,
// with no content_block_delta event.
var latencyNoDeltaTurn = []string{
	`{"type":"message_start","message":{"id":"msg_03","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":20,"output_tokens":5}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"{\"answer\":\"42\"}"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
	`{"type":"message_stop"}`,
}

// requireStreamedLatency checks the timing every streamed turn carries and
// returns it.
func requireStreamedLatency(t *testing.T, turn agenttrace.RecordedTurn) agenttrace.TurnLatency {
	t.Helper()
	if turn.Latency == nil {
		t.Fatalf("turn %d: Latency = nil, want the answering attempt's timing", turn.Index)
	}
	l := *turn.Latency
	if err := l.Validate(); err != nil {
		t.Errorf("turn %d: Latency.Validate(): %v", turn.Index, err)
	}
	if l.RequestStartTime.IsZero() || l.ConnReused == nil || l.FirstByteMS == nil {
		t.Errorf("turn %d: Latency = %+v, want start time, reuse flag, and first byte", turn.Index, l)
	}
	if l.FirstTokenMS == nil {
		t.Errorf("turn %d: FirstTokenMS = nil, want the first content_block_delta", turn.Index)
	}
	return l
}

func TestExecutorRecordsStreamedCallLatency(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(reqNum int, _ []byte) []string {
		if reqNum == 1 {
			return latencyToolCallTurn
		}
		return latencyFinalTurn
	})
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools := map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": true}
			},
		},
	}
	tracer := &recordingTracer{}
	if _, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, tools); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	turns := tracer.traces[0].Turns
	if len(turns) != 2 {
		t.Fatalf("turns: got = %d, want = 2", len(turns))
	}

	first := requireStreamedLatency(t, turns[0])
	if *first.ConnReused {
		t.Error("turn 0: ConnReused = true, want false on a fresh server")
	}
	if first.ConnectMS == nil {
		t.Error("turn 0: ConnectMS = nil, want the dial time")
	}
	if first.TLSMS != nil {
		t.Errorf("turn 0: TLSMS = %d, want nil over plain HTTP", *first.TLSMS)
	}

	second := requireStreamedLatency(t, turns[1])
	if !*second.ConnReused || second.ConnectMS != nil {
		t.Errorf("turn 1: ConnReused = %t, ConnectMS = %v, want a reused connection with no dial", *second.ConnReused, second.ConnectMS)
	}
	if !second.RequestStartTime.After(first.RequestStartTime) {
		t.Errorf("turn 1: RequestStartTime = %v, want after turn 0's %v", second.RequestStartTime, first.RequestStartTime)
	}
}

// The latency belongs to the attempt that answered, not to an attempt the
// executor retried.
func TestExecutorRecordsAnsweringAttemptLatency(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		n := len(arrivals)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"upstream 503"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, latencyFinalTurn))
	}))
	t.Cleanup(srv.Close)
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(1)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracer := &recordingTracer{}
	if _, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	turns := tracer.traces[0].Turns
	if len(turns) != 1 || len(turns[0].Errors) == 0 {
		t.Fatalf("turns = %+v, want one turn that recorded the retried 503", turns)
	}
	got := requireStreamedLatency(t, turns[0])
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 2 {
		t.Fatalf("requests: got = %d, want = 2", len(arrivals))
	}
	if !got.RequestStartTime.After(arrivals[0]) || got.RequestStartTime.After(arrivals[1]) {
		t.Errorf("RequestStartTime = %v, want between the retried request (%v) and the answering one (%v)", got.RequestStartTime, arrivals[0], arrivals[1])
	}
}

// Only a content_block_delta marks the first token: a stream without one, here
// a block carried whole by content_block_start, leaves FirstTokenMS unset.
func TestExecutorLeavesFirstTokenUnsetWithoutDeltas(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string { return latencyNoDeltaTurn })
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracer := &recordingTracer{}
	got, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Answer != "42" {
		t.Fatalf("Answer: got = %q, want = %q", got.Answer, "42")
	}
	l := tracer.traces[0].Turns[0].Latency
	if l == nil || l.FirstByteMS == nil {
		t.Fatalf("Latency = %+v, want the call's timing with a first byte", l)
	}
	if l.FirstTokenMS != nil {
		t.Errorf("FirstTokenMS: got = %d, want nil without a content_block_delta", *l.FirstTokenMS)
	}
}
