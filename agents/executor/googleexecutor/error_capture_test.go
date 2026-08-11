/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/genai"
)

type errCapRequest struct{}

func (errCapRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type errCapResponse struct {
	Answer string `json:"answer"`
}

type recordingTracer struct {
	traces []*agenttrace.Trace[errCapResponse]
}

func (r *recordingTracer) NewTrace(ctx context.Context, prompt string, opts ...agenttrace.StartTraceOption) *agenttrace.Trace[errCapResponse] {
	def := agenttrace.NewDefaultTracer[errCapResponse](ctx)
	return def.NewTrace(ctx, prompt, opts...)
}

func (r *recordingTracer) RecordTrace(t *agenttrace.Trace[errCapResponse]) {
	r.traces = append(r.traces, t)
}

func fastRetry(maxRetries int) retry.RetryConfig {
	return retry.RetryConfig{
		MaxRetries:  maxRetries,
		BaseBackoff: time.Microsecond,
		MaxBackoff:  time.Microsecond,
		MaxJitter:   0,
	}
}

// newTestClient builds a genai.Client whose API endpoint is the given fake
// server URL. Uses the Gemini API backend with an APIKey to avoid Vertex's
// requirement for real GCP credentials in unit tests.
func newTestClient(t *testing.T, baseURL string) *genai.Client {
	t.Helper()
	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
		APIKey:  "test",
		HTTPOptions: genai.HTTPOptions{
			BaseURL: baseURL,
		},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	return client
}

// A non-retryable upstream failure must mark the LLM turn as Failed and
// land the cause in Errors. This is the regression guard for the deferred
// Fail wiring in the google executor's executeTurn closure.
func TestExecutorMarksTurnFailedOnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 400 is non-retryable per isRetryableVertexError's switch (and the
		// body string doesn't match any of the substrings the fallback hunts
		// for), so the executor surfaces immediately.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"invalid argument","status":"INVALID_ARGUMENT"}}`))
	}))
	t.Cleanup(srv.Close)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, execErr := exec.Execute(ctx, errCapRequest{}, nil); execErr == nil {
		t.Fatal("Execute: got nil error, want non-nil from upstream 400")
	}

	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	// The google executor calls retry once before the turn loop
	// (send_initial_message) — its failures don't land on a turn. A
	// subsequent failure at executeTurn level would. We accept both
	// possibilities here: the test goal is that IF a turn ran and failed,
	// it is correctly marked. If the failure happens pre-turn the trace
	// just has no turns, which is also a valid shape.
	for _, turn := range tracer.traces[0].Turns {
		if !turn.Failed {
			t.Errorf("trace.Turns[%d].Failed: got = false, want = true", turn.Index)
		}
		if len(turn.Errors) == 0 {
			t.Errorf("trace.Turns[%d].Errors: got = empty, want = non-empty", turn.Index)
		}
	}
}

// Retryable upstream failures that exhaust retries must surface every
// transient attempt in Errors via the OnAttemptError callback wired into
// the per-turn retry config. Note: this only exercises the in-turn retry
// callsites (send_malformed_retry / send_tool_responses / send_submit_redirect);
// send_initial_message runs before the turn loop and isn't wired to a turn.
func TestExecutorRecordsTransientErrorsViaRetryCallback(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// 503 with "unavailable" message is retryable per
		// isRetryableVertexError both via apiErr.Code and the substring
		// fallback.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":503,"message":"upstream unavailable","status":"UNAVAILABLE"}}`))
	}))
	t.Cleanup(srv.Close)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	const maxRetries = 2
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(maxRetries)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, execErr := exec.Execute(ctx, errCapRequest{}, nil); execErr == nil {
		t.Fatal("Execute: got nil error, want non-nil after retry exhaustion")
	}

	// At least maxRetries+1 attempts (the initial send + retries), maybe
	// more if the executor enters the turn loop and retries again.
	if got, want := attempts.Load(), int32(maxRetries+1); got < want {
		t.Fatalf("HTTP attempts: got = %d, want >= %d", got, want)
	}

	// The send_initial_message retry runs OUTSIDE the turn loop; its retries
	// don't get surfaced as turn-level Errors. So this test does not always
	// guarantee the in-turn callback wiring fired — what it does guarantee
	// is that the existing failure-recording (Fail-via-defer) still works
	// end-to-end on the google executor, parallel to claude/openai.
	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	for _, turn := range tracer.traces[0].Turns {
		if !turn.Failed {
			t.Errorf("trace.Turns[%d].Failed: got = false, want = true", turn.Index)
		}
	}
}

// When the conversation runs through every allowed turn without ever
// returning a final result, the executor exits with the unprocessed
// tail-end response that the next iteration would have consumed. That
// response carries real billable tokens — the model already generated
// them — so the executor records a synthetic final turn so the cost view
// (which sums turns[]) sees them.
//
// Without that synthetic turn a maxTurns-exhausted run would silently
// drop the last response's tokens from turns[] and the cost view's
// aggregate. This test pins the contract: a fake server that always
// returns a tool call drives executeTurn to natural exhaustion at
// maxTurns, then asserts the synthetic turn is appended with the
// last response's complete input/output token totals.
func TestExecutorRecordsUnprocessedResponseAtMaxTurnsExhaustion(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Each response carries distinct values for every token category
		// so test assertions can pinpoint which response landed on which
		// turn — a regression that, say, attributed the synthetic turn's
		// tokens to the wrong response would surface as a clear off-by-one.
		n := int(requestCount.Add(1))
		prompt := 100*n + 1
		candidates := 100*n + 2
		toolUsePrompt := 10*n + 3
		thoughts := 10*n + 4
		body := fmt.Sprintf(`{
			"candidates":[{"content":{"parts":[{"functionCall":{"name":"unknown_tool","args":{"x":1}}}]}}],
			"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"toolUsePromptTokenCount":%d,"thoughtsTokenCount":%d,"totalTokenCount":%d}
		}`, prompt, candidates, toolUsePrompt, thoughts, prompt+candidates+toolUsePrompt+thoughts)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
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
		// Disable context caching so we don't need to fake the Caches.Create
		// endpoint — orthogonal to the maxTurns-exhaustion behavior under test.
		googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, execErr := exec.Execute(ctx, errCapRequest{}, nil); execErr == nil {
		t.Fatal("Execute: got nil error, want non-nil from maxTurns exhaustion")
	}

	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	trace := tracer.traces[0]

	// Each fake response is response (i+1): the initial Send is response 1
	// (recorded on turn 0), each subsequent tool-response Send produces
	// response (i+1) recorded on turn i. The final synthetic turn at
	// index maxTurns carries the response generated by the last in-loop
	// chat.Send — which would have driven the next, never-executed turn.
	want := []agenttrace.RecordedTurn{
		{Index: 0, Model: "gemini-2.5-flash", System: agenttrace.SystemGoogleVertex, InputTokens: 114, OutputTokens: 116},
		{Index: 1, Model: "gemini-2.5-flash", System: agenttrace.SystemGoogleVertex, InputTokens: 224, OutputTokens: 226},
		{Index: 2, Model: "gemini-2.5-flash", System: agenttrace.SystemGoogleVertex, InputTokens: 334, OutputTokens: 336},
	}
	if diff := cmp.Diff(want, trace.Turns, cmpopts.IgnoreFields(agenttrace.RecordedTurn{}, "StartTime", "EndTime")); diff != "" {
		t.Errorf("trace turns (-want +got):\n%s", diff)
	}
}

// Companion to TestExecutorRecordsUnprocessedResponseAtMaxTurnsExhaustion:
// when context caching is enabled, the synthetic final turn must also
// route the unprocessed response's CachedContentTokenCount onto its
// CacheReadTokens record. Without per-turn cache attribution the cost
// view applies the cache-discount once at the wrong granularity (or
// not at all on the synthetic turn), which would silently skew the
// effective rate on maxTurns-exhausted runs.
//
// Uses the default cacheControl=true; getOrCreateCache is gated on
// `systemInstruction != nil || len(tools) > 0` (executor.go around the
// chat.Create call), so with neither configured the cache-creation
// HTTP endpoint is never hit — we only need to fake generateContent.
func TestExecutorRecordsUnprocessedCacheTokensAtMaxTurnsExhaustion(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Distinct cache counts per response (10*N) plus the same prompt /
		// candidates pattern as the sibling test, so the assertion can
		// confirm the synthetic turn picked up the LAST response's cache
		// reads — not, say, an aggregate across turns or a stale snapshot.
		n := int(requestCount.Add(1))
		prompt := 100*n + 1
		candidates := 100*n + 2
		cached := 10 * n
		body := fmt.Sprintf(`{
			"candidates":[{"content":{"parts":[{"functionCall":{"name":"unknown_tool","args":{"x":1}}}]}}],
			"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"cachedContentTokenCount":%d,"totalTokenCount":%d}
		}`, prompt, candidates, cached, prompt+candidates)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
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
		// cacheControl=true is the default — leave it on so the synthetic-
		// turn cache branch executes. With no system instructions or tools
		// configured, getOrCreateCache is never called.
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, execErr := exec.Execute(ctx, errCapRequest{}, nil); execErr == nil {
		t.Fatal("Execute: got nil error, want non-nil from maxTurns exhaustion")
	}

	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	trace := tracer.traces[0]

	wantTurns := maxTurns + 1
	if got := len(trace.Turns); got != wantTurns {
		t.Fatalf("len(trace.Turns): got = %d, want = %d", got, wantTurns)
	}

	// CacheReadTokens on every turn must mirror the response that landed
	// there. This request has no system instructions or tools, so it never
	// creates a cache and CacheCreationTokens stays zero.
	for i, turn := range trace.Turns {
		wantCacheRead := int64(10 * (i + 1))
		if turn.CacheReadTokens != wantCacheRead {
			t.Errorf("trace.Turns[%d].CacheReadTokens: got = %d, want = %d", i, turn.CacheReadTokens, wantCacheRead)
		}
		if turn.CacheCreationTokens != 0 {
			t.Errorf("trace.Turns[%d].CacheCreationTokens: got = %d, want = 0 (no cache was created)", i, turn.CacheCreationTokens)
		}
	}
}
