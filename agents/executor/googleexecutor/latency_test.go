/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// Turn N sends request N and then processes its response. Each turn's latency
// must be that call's: the request that started after the previous response
// arrived and before this one did. The run exhausts its turns, so the
// synthetic last turn is covered too.
func TestExecutorRecordsLatencyWithItsResponse(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"candidates":[{"content":{"parts":[{"functionCall":{"name":"unknown_tool","args":{"x":1}}}]}}],
			"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
		}`)
	}))
	t.Cleanup(srv.Close)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	const maxTurns = 2
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](maxTurns),
		googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracer := &recordingTracer{}
	if _, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil); err == nil {
		t.Fatal("Execute: got nil error, want maxTurns exhaustion")
	}

	turns := tracer.traces[0].Turns
	mu.Lock()
	defer mu.Unlock()
	if len(turns) != maxTurns+1 || len(arrivals) != maxTurns+1 {
		t.Fatalf("turns = %d, requests = %d, want %d of each", len(turns), len(arrivals), maxTurns+1)
	}
	for i, turn := range turns {
		if turn.Latency == nil {
			t.Errorf("turn %d: Latency = nil, want its response's call timing", i)
			continue
		}
		l := *turn.Latency
		if err := l.Validate(); err != nil {
			t.Errorf("turn %d: Latency.Validate(): %v", i, err)
		}
		if l.ConnReused == nil || l.FirstByteMS == nil {
			t.Errorf("turn %d: Latency = %+v, want reuse flag and first byte", i, l)
		}
		if l.FirstTokenMS != nil {
			t.Errorf("turn %d: FirstTokenMS = %d, want nil for a call that does not stream", i, *l.FirstTokenMS)
		}
		if l.RequestStartTime.After(arrivals[i]) || (i > 0 && !l.RequestStartTime.After(arrivals[i-1])) {
			t.Errorf("turn %d: RequestStartTime = %v, want the call that produced response %d (arrived %v)", i, l.RequestStartTime, i+1, arrivals[i])
		}
	}
}
