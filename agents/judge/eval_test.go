/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	noop "go.opentelemetry.io/otel/trace/noop"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/judge"
)

// captureJudge records the context Judge is called with so tests can assert
// on the values propagated through the eval callback chain. The judgment it
// returns is fixed and uninteresting; the point is the ctx it observed.
type captureJudge struct {
	gotCtx context.Context
}

func (c *captureJudge) Judge(ctx context.Context, _ *judge.Request) (*judge.Judgement, error) {
	c.gotCtx = ctx
	return &judge.Judgement{Score: 1.0, Reasoning: "ok"}, nil
}

// Mock types are defined in testhelpers_test.go

func TestNewGoldenEval(t *testing.T) {
	// Create mock judge
	mockJudgment := &judge.Judgement{
		Score:     0.85,
		Reasoning: "Good match with minor differences",
		Suggestions: []string{
			"Consider being more specific",
			"Add more detail",
		},
	}
	judgeImpl := &mockJudge{judgment: mockJudgment}

	// Create the eval callback
	evalCallback := judge.NewGoldenEval[*judge.Judgement](judgeImpl, "correctness", "Expected answer")

	// Create a test trace
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "What is 2+2?",
		Result: &judge.Judgement{
			Score:     0.9,
			Reasoning: "The answer is correct",
		},
	}

	// Create mock observer
	obs := &mockObserver{}

	// Run the eval
	evalCallback(obs, trace)

	// Check logs
	logs := obs.getLogs()
	if len(logs) == 0 {
		t.Error("log count: got = 0, wanted = > 0")
	}

	// Verify key log messages
	expectedLogs := []string{
		"Grade: 0.85",
		"Good match with minor differences",
		"Suggestion: Consider being more specific",
		"Suggestion: Add more detail",
	}

	for _, expected := range expectedLogs {
		found := false
		for _, log := range logs {
			if strings.Contains(log, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("log content: got = %v, wanted = containing %q", logs, expected)
		}
	}
}

func TestNewEvalWithError(t *testing.T) {
	// Create mock judge that returns error
	judgeImpl := &mockJudge{err: errors.New("API error")}

	// Create the eval callback
	evalCallback := judge.NewGoldenEval[*judge.Judgement](judgeImpl, "accuracy", "Expected answer")

	// Create a test trace
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "Test prompt",
		Result: &judge.Judgement{
			Score:     0.5,
			Reasoning: "Test reasoning",
		},
	}

	// Create mock observer
	obs := &mockObserver{}

	// Run the eval
	evalCallback(obs, trace)

	// A judge transport/parse error is not a quality signal: after retrying,
	// the eval skips the metric rather than recording a failure or a grade, so
	// a judge-infra blip cannot fail the test.
	if failures := obs.getFailures(); len(failures) != 0 {
		t.Errorf("failures on judge error: got = %v, want = none (metric skipped)", failures)
	}
	if grades := obs.getGrades(); len(grades) != 0 {
		t.Errorf("grades on judge error: got = %v, want = none (metric skipped)", grades)
	}

	// The judge call is retried before giving up, so it is invoked more than
	// once.
	if calls := judgeImpl.callCount(); calls <= 1 {
		t.Errorf("judge call count: got = %d, want = > 1 (retried before skip)", calls)
	}
}

func TestNewEvalWithNilResult(t *testing.T) {
	judgeImpl := &mockJudge{}
	evalCallback := judge.NewGoldenEval[*judge.Judgement](judgeImpl, "completeness", "Expected")

	// Create trace with nil result
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "Test prompt",
		Result:      nil,
	}

	obs := &mockObserver{}
	evalCallback(obs, trace)

	// Should fail on nil result extraction
	logs := obs.getLogs()
	found := false
	for _, log := range logs {
		if strings.Contains(log, "Failed to extract response") && strings.Contains(log, "trace has no result") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("extraction failure log: got = %v, wanted = containing 'Failed to extract response' and 'trace has no result'", logs)
	}
}

func TestNewStandaloneEval(t *testing.T) {
	// Create mock judge
	mockJudgment := &judge.Judgement{
		Score:     0.8,
		Reasoning: "Response is clear and well-structured",
		Suggestions: []string{
			"Add more specific examples",
		},
	}
	judgeImpl := &mockJudge{judgment: mockJudgment}

	// Create the eval callback
	evalCallback := judge.NewStandaloneEval[*judge.Judgement](judgeImpl, "clarity - response should be easy to understand")

	// Create a test trace
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "Test prompt",
		Result: &judge.Judgement{
			Score:     0.7,
			Reasoning: "Test response for evaluation",
		},
	}

	// Create mock observer
	obs := &mockObserver{}

	// Run the eval
	evalCallback(obs, trace)

	// Check logs
	logs := obs.getLogs()
	if len(logs) == 0 {
		t.Error("log count: got = 0, wanted = > 0")
	}

	// Verify key log messages
	expectedLogs := []string{
		"Grade: 0.80",
		"Response is clear and well-structured",
		"Suggestion: Add more specific examples",
	}

	for _, expected := range expectedLogs {
		found := false
		for _, log := range logs {
			if strings.Contains(log, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("log content: got = %v, wanted = containing %q", logs, expected)
		}
	}
}

// TestEvalInheritsParentCtxValues verifies that an eval callback invoked on
// a completed trace receives a context that inherits the parent's
// WithDefaultAgentName / WithDefaultNameFn / WithExecutionContext / custom
// values, while detaching from the parent's cancellation chain. Without
// this, every eval invocation runs against context.Background() and emits
// spans that have no link back to the reconciler that produced the trace.
//
// Covers both NewStandaloneEval and NewGoldenEval — they share the same
// ctx-propagation code path, so a regression in either entry point should
// fail this test.
func TestEvalInheritsParentCtxValues(t *testing.T) {
	type customKey struct{}

	// buildParent constructs a parent ctx carrying everything a reconciler
	// would set: agent name (static gen_ai.agent.name fallback), name fn
	// (dynamic per-resource span label), execution context (PR/commit
	// metadata), payloads opt-in, and a custom value to prove arbitrary
	// context values survive too. Returned alongside its cancel func so
	// each subtest can cancel its own parent ctx independently.
	buildParent := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(t.Context())
		ctx = agenttrace.WithDefaultAgentName(ctx, "test-reconciler")
		ctx = agenttrace.WithDefaultNameFn(ctx, func(ec agenttrace.ExecutionContext) string {
			return "test-rec: " + ec.ReconcilerKey
		})
		ctx = agenttrace.WithExecutionContext(ctx, agenttrace.ExecutionContext{
			ReconcilerKey:  "pr:owner/repo/42",
			ReconcilerType: "pr",
			CommitSHA:      "deadbeef",
		})
		ctx = agenttrace.WithPayloadsEnabled(ctx, true)
		ctx = context.WithValue(ctx, customKey{}, "custom-value")
		return ctx, cancel
	}

	cases := []struct {
		name string
		run  func(j judge.Interface) func(*mockObserver, *agenttrace.Trace[*judge.Judgement])
	}{
		{
			name: "NewStandaloneEval",
			run: func(j judge.Interface) func(*mockObserver, *agenttrace.Trace[*judge.Judgement]) {
				cb := judge.NewStandaloneEval[*judge.Judgement](j, "criterion")
				return func(o *mockObserver, tr *agenttrace.Trace[*judge.Judgement]) { cb(o, tr) }
			},
		},
		{
			name: "NewGoldenEval",
			run: func(j judge.Interface) func(*mockObserver, *agenttrace.Trace[*judge.Judgement]) {
				cb := judge.NewGoldenEval[*judge.Judgement](j, "criterion", "golden")
				return func(o *mockObserver, tr *agenttrace.Trace[*judge.Judgement]) { cb(o, tr) }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parentCtx, cancelParent := buildParent()
			defer cancelParent()

			// StartTrace seeds Trace.ctx with parentCtx so the eval can recover
			// it via trace.Context(). done() finalises the trace; eval
			// callbacks fire after Complete in the real flow.
			tr, done := agenttrace.StartTrace[*judge.Judgement](parentCtx, "prompt-"+tc.name)
			done(&judge.Judgement{Score: 0.5}, nil)

			// Cancel the parent ctx *before* the eval runs. The eval must
			// still see a live ctx — long evals that outlive the request that
			// produced the trace must not abort on parent cancellation.
			cancelParent()

			capt := &captureJudge{}
			obs := &mockObserver{}
			tc.run(capt)(obs, tr)

			if capt.gotCtx == nil {
				t.Fatal("judge was not invoked or did not capture ctx")
			}

			// Inherited values: agent name, name fn (and its output once
			// invoked), execution context, custom value.
			if got, want := agenttrace.GetDefaultAgentName(capt.gotCtx), "test-reconciler"; got != want {
				t.Errorf("agent name: got %q, want %q", got, want)
			}
			fn := agenttrace.GetDefaultNameFn(capt.gotCtx)
			if fn == nil {
				t.Fatal("name fn missing from eval ctx")
			}
			if got, want := fn(agenttrace.GetExecutionContext(capt.gotCtx)), "test-rec: pr:owner/repo/42"; got != want {
				t.Errorf("name fn output: got %q, want %q", got, want)
			}
			if got, want := agenttrace.GetExecutionContext(capt.gotCtx).ReconcilerKey, "pr:owner/repo/42"; got != want {
				t.Errorf("execution context ReconcilerKey: got %q, want %q", got, want)
			}
			if got, want := capt.gotCtx.Value(customKey{}), "custom-value"; got != want {
				t.Errorf("custom value: got %v, want %q", got, want)
			}

			// Detached cancellation: parent ctx was cancelled above; the
			// eval's ctx must still be live.
			if err := capt.gotCtx.Err(); err != nil {
				t.Errorf("eval ctx Err: got %v, want nil (cancellation must not propagate)", err)
			}
		})
	}
}

// TestEvalFallsBackToBackgroundForNilCtx pins the defensive fallback in
// the eval entry points: traces constructed as struct literals (as in
// many existing unit tests) have a nil internal ctx, and the eval must
// not panic.
func TestEvalFallsBackToBackgroundForNilCtx(t *testing.T) {
	tr := &agenttrace.Trace[*judge.Judgement]{Result: &judge.Judgement{Score: 0.5}}

	capt := &captureJudge{}
	cb := judge.NewStandaloneEval[*judge.Judgement](capt, "criterion")
	cb(&mockObserver{}, tr)

	if capt.gotCtx == nil {
		t.Fatal("eval ctx is nil; fallback to context.Background() did not engage")
	}
	if err := capt.gotCtx.Err(); err != nil {
		t.Errorf("eval ctx Err: got %v, want nil", err)
	}
	if got := agenttrace.GetDefaultAgentName(capt.gotCtx); got != "" {
		t.Errorf("agent name on fallback ctx: got %q, want empty", got)
	}
	if fn := agenttrace.GetDefaultNameFn(capt.gotCtx); fn != nil {
		t.Errorf("name fn on fallback ctx: got non-nil, want nil")
	}
}

// emittingJudge mimics the trace-emitting behaviour of real judge
// implementations (google.go, claude.go): override the agent name so the
// judge's own span carries gen_ai.agent.name="judge", then call StartTrace
// to emit an invoke_agent span via the global OTel tracer. A test driving
// this through the eval callback can then inspect the emitted spans to
// confirm trace-tree integrity across the eval boundary.
type emittingJudge struct{}

func (e *emittingJudge) Judge(ctx context.Context, _ *judge.Request) (*judge.Judgement, error) {
	ctx = agenttrace.WithDefaultAgentName(ctx, "judge")
	_, done := agenttrace.StartTrace[*judge.Judgement](ctx, "judge-prompt")
	done(&judge.Judgement{Score: 1.0, Reasoning: "ok"}, nil)
	return &judge.Judgement{Score: 1.0, Reasoning: "ok"}, nil
}

// installSpanRecorder installs a SpanRecorder as the global TracerProvider
// for the duration of the test and returns it. Any span started via
// otel.Tracer(...) during the test lands in the recorder. On cleanup the
// global provider is reset to the noop implementation so other tests see
// a span context with no trace ID. Inlined here because agenttrace's
// equivalent helper is internal to its _test package.
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return sr
}

// findSpanByAgent returns the first ended span whose gen_ai.agent.name
// attribute matches the given name, or nil. Lookup is by attribute rather
// than span name because the executor-set name ("invoke_agent" until a
// turn renames it) doesn't disambiguate between the parent trace's span
// and the judge-emitted span.
func findSpanByAgent(spans []sdktrace.ReadOnlySpan, agent string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		for _, a := range s.Attributes() {
			if string(a.Key) == "gen_ai.agent.name" && a.Value.AsString() == agent {
				return s
			}
		}
	}
	return nil
}

// TestEvalEmitsSpansInParentTraceTree verifies that a judge-style eval
// callback, when it does its own StartTrace inside Judge(), emits a span
// that is correctly parented under the trace that triggered the eval.
// This is the load-bearing guarantee: a regression in ctx propagation
// would surface here as two disconnected root spans rather than a
// well-formed two-span tree.
//
// Assertions stay on OTel-standard attributes (gen_ai.agent.name) and
// core span identity (TraceID equality, Parent SpanID linkage) so the
// test reads as a pure tracing-protocol assertion independent of any
// downstream observability platform.
func TestEvalEmitsSpansInParentTraceTree(t *testing.T) {
	sr := installSpanRecorder(t)

	// Parent ctx: agent name distinguishes the outer reconciler span from
	// the inner judge span; nameFn and exec context are along for the ride
	// to mirror a real reconciler entry point.
	parentCtx := agenttrace.WithDefaultAgentName(t.Context(), "test-reconciler")
	parentCtx = agenttrace.WithDefaultNameFn(parentCtx, func(ec agenttrace.ExecutionContext) string {
		return "test-rec: " + ec.ReconcilerKey
	})
	parentCtx = agenttrace.WithExecutionContext(parentCtx, agenttrace.ExecutionContext{
		ReconcilerKey:  "pr:owner/repo/42",
		ReconcilerType: "pr",
	})

	// Drive the parent trace: StartTrace emits the parent invoke_agent
	// span; done() ends it. Eval callbacks fire after this point in
	// production.
	parentTrace, parentDone := agenttrace.StartTrace[*judge.Judgement](parentCtx, "parent-prompt")
	parentDone(&judge.Judgement{Score: 0.9}, nil)

	// Run the eval callback. The emittingJudge inside it does its own
	// StartTrace, which (with the fix in place) inherits the parent
	// ctx's span as its OTel parent.
	cb := judge.NewStandaloneEval[*judge.Judgement](&emittingJudge{}, "criterion")
	cb(&mockObserver{}, parentTrace)

	spans := sr.Ended()
	if got := len(spans); got != 2 {
		t.Fatalf("recorded span count: got %d, want 2 (parent + judge)", got)
	}

	parent := findSpanByAgent(spans, "test-reconciler")
	if parent == nil {
		t.Fatal("parent span not found by gen_ai.agent.name=test-reconciler")
	}
	judgeSpan := findSpanByAgent(spans, "judge")
	if judgeSpan == nil {
		t.Fatal("judge span not found by gen_ai.agent.name=judge")
	}

	// Same trace: both spans must share a TraceID. Two distinct TraceIDs
	// would indicate the judge span came up as its own orphan root rather
	// than a child of the parent — the regression this test pins.
	if pt, jt := parent.SpanContext().TraceID(), judgeSpan.SpanContext().TraceID(); pt != jt {
		t.Errorf("trace id: parent=%s judge=%s, want equal", pt, jt)
	}

	// Span tree: judge.Parent().SpanID must equal parent's SpanID. This
	// is the explicit parent-child link that a span recorder can prove
	// even though the parent span has already ended by the time the
	// child starts (eval callbacks fire post-Complete).
	if got, want := judgeSpan.Parent().SpanID(), parent.SpanContext().SpanID(); got != want {
		t.Errorf("judge span parent: got %s, want %s (parent's span id)", got, want)
	}
}
