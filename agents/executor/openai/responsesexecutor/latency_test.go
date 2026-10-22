/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

// The Responses executor streams, so each turn also records the time to the
// first streamed delta.
func TestTraceRecordsStreamedCallLatency(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	e := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			emit(w, call("read", "read_fixture", `{"reasoning":"inspect"}`))
			return
		}
		emit(w, submit("submit", "done"))
	}, config(t))
	tools := map[string]toolcall.Tool[answer]{"read_fixture": {
		Def: toolcall.Definition{Name: "read_fixture"},
		Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[answer], *answer) map[string]any {
			return map[string]any{"fixture": "safe"}
		},
	}}
	tracer := new(recordingTracer)
	if _, err := e.Execute(agenttrace.WithTracer[answer](t.Context(), tracer), request{}, tools); err != nil {
		t.Fatal(err)
	}
	turns := tracer.trace.Turns
	if len(turns) != 2 {
		t.Fatalf("turns: got = %d, want = 2", len(turns))
	}
	for i, wantReused := range []bool{false, true} {
		l := turns[i].Latency
		if l == nil {
			t.Fatalf("turn %d: Latency = nil, want the answering attempt's timing", i)
		}
		if err := l.Validate(); err != nil {
			t.Errorf("turn %d: Latency.Validate(): %v", i, err)
		}
		if l.RequestStartTime.IsZero() || l.FirstByteMS == nil {
			t.Errorf("turn %d: Latency = %+v, want start time and first byte", i, *l)
		}
		if l.ConnReused == nil || *l.ConnReused != wantReused {
			t.Errorf("turn %d: ConnReused = %v, want %t", i, l.ConnReused, wantReused)
		}
		if l.FirstTokenMS == nil {
			t.Errorf("turn %d: FirstTokenMS = nil, want the first streamed delta", i)
		}
	}
}

// Only a *.delta event marks the first token: a stream that carries its output
// in response.completed alone leaves FirstTokenMS unset.
func TestTraceLeavesFirstTokenUnsetWithoutDeltas(t *testing.T) {
	t.Parallel()
	e := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"in_progress\"}}\n\n")
		b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_fixture", "status": "completed", "output": []any{submit("submit", "done")},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13},
		}})
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", b)
	}, config(t))
	tracer := new(recordingTracer)
	if _, err := e.Execute(agenttrace.WithTracer[answer](t.Context(), tracer), request{}, nil); err != nil {
		t.Fatal(err)
	}
	l := tracer.trace.Turns[0].Latency
	if l == nil || l.FirstByteMS == nil {
		t.Fatalf("Latency = %+v, want the call's timing with a first byte", l)
	}
	if l.FirstTokenMS != nil {
		t.Errorf("FirstTokenMS: got = %d, want nil without a delta event", *l.FirstTokenMS)
	}
}
