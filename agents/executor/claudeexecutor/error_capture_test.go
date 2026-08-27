/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// errCapRequest is a minimal Bindable so we can drive the executor with a
// fixed prompt that doesn't depend on input.
type errCapRequest struct{}

// fastRetry is a retry config that minimizes wall-clock cost for hermetic
// tests that deliberately exhaust retries.
func fastRetry(maxRetries int) retry.RetryConfig {
	return retry.RetryConfig{
		MaxRetries:  maxRetries,
		BaseBackoff: time.Microsecond,
		MaxBackoff:  time.Microsecond,
		MaxJitter:   0,
	}
}

func (errCapRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type errCapResponse struct {
	Answer string `json:"answer"`
}

// recordingTracer captures every completed Trace in-memory so tests can
// inspect what landed on RecordedTurn — it stands in for the production
// CloudEvent recorder without requiring eventing infrastructure.
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

// A non-retryable API failure (401) must mark the LLM turn as Failed and
// land the error string in the chronological Errors list. This is the
// regression guard for the deferred Fail(err) wiring inside executeTurn —
// a future refactor that loses the named-return contract would surface
// here as Failed=false on a turn that actually failed.
func TestExecutorMarksTurnFailedOnAPIError(t *testing.T) {
	// Fake Anthropic API returning a non-retryable 401 so isRetryableClaudeError
	// rejects it and the executor surfaces the failure on the first attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0), // Disable SDK-level retries so the test runs immediately.
	)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := claudeexecutor.NewWithMessages[errCapRequest, errCapResponse](
		client.Messages,
		prompt,
		// Single attempt: the failure is non-retryable, but pinning MaxRetries=0
		// keeps the test fast even if the retryability classification regresses.
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, execErr := exec.Execute(ctx, errCapRequest{}, nil); execErr == nil {
		t.Fatal("Execute: got nil error, want non-nil from upstream 401")
	}

	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	trace := tracer.traces[0]
	if len(trace.Turns) != 1 {
		t.Fatalf("trace.Turns: got = %d, want = 1", len(trace.Turns))
	}

	got := trace.Turns[0]
	if !got.Failed {
		t.Errorf("trace.Turns[0].Failed: got = false, want = true (executor wiring did not call Fail)")
	}
	if len(got.Errors) == 0 {
		t.Errorf("trace.Turns[0].Errors: got = empty, want = non-empty (executor wiring did not record the cause)")
	}
}

// A retryable upstream error that exhausts retries must surface every
// transient attempt in the turn's Errors list (via the OnAttemptError
// callback wired into the per-turn retry config) AND the final wrapped
// terminal error (via Fail). This is the regression guard for the retry
// callback wiring — without it, the transient errors that the retry layer
// observes would never reach the BQ row, and the recovery-shape that
// motivated the events-list model would be unreachable.
func TestExecutorRecordsTransientErrorsViaRetryCallback(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"upstream 503"}}`))
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0), // Disable SDK-level retries so the executor's retry layer is the only one in play.
	)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	const maxRetries = 2
	exec, err := claudeexecutor.NewWithMessages[errCapRequest, errCapResponse](
		client.Messages,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(maxRetries)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, execErr := exec.Execute(ctx, errCapRequest{}, nil); execErr == nil {
		t.Fatal("Execute: got nil error, want non-nil after retry exhaustion")
	}

	// Sanity: the executor actually exercised the retry loop.
	if got, want := attempts.Load(), int32(maxRetries+1); got != want {
		t.Fatalf("HTTP attempts: got = %d, want = %d", got, want)
	}

	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 1 {
		t.Fatalf("expected exactly one trace with one turn, got %d traces", len(tracer.traces))
	}
	got := tracer.traces[0].Turns[0]
	if !got.Failed {
		t.Error("trace.Turns[0].Failed: got = false, want = true")
	}
	// Each retried attempt fires OnAttemptError once; the final exhausted
	// attempt's wrapped err comes through Fail. With MaxRetries=2 that's
	// 2 transients + 1 terminal = 3 entries. Allow >= 3 to tolerate any
	// future SDK behavior that might surface intermediate framing errors.
	if got, want := len(got.Errors), 3; got < want {
		t.Errorf("trace.Turns[0].Errors length: got = %d, want >= %d", got, want)
	}
}
