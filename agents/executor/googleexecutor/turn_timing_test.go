/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/genai"
)

// toolDelay is how long slowTool takes to run.
const toolDelay = 20 * time.Millisecond

// scriptedCall is one model call the timed model answers: how long the call
// takes, and what it replies with.
type scriptedCall struct {
	delay time.Duration
	body  string
	// unavailable replies 503 UNAVAILABLE instead of body: a transient error
	// that the executor retries.
	unavailable bool
}

// callWindow is when the timed model received a call and when it replied.
type callWindow struct {
	start, end time.Time
}

// timedModel is an in-memory Gemini endpoint whose calls take a known time.
// Inside a synctest bubble the delay advances the fake clock by exactly that
// much, so tests can assert exact turn durations.
type timedModel struct {
	t      *testing.T
	script []scriptedCall

	mu    sync.Mutex
	calls []callWindow
}

func (m *timedModel) client() *genai.Client {
	m.t.Helper()
	client, err := genai.NewClient(m.t.Context(), &genai.ClientConfig{
		Backend:     genai.BackendGeminiAPI,
		APIKey:      "test",
		HTTPOptions: genai.HTTPOptions{BaseURL: "http://gemini.test"},
		HTTPClient:  &http.Client{Transport: roundTripFunc(m.roundTrip)},
	})
	if err != nil {
		m.t.Fatalf("genai.NewClient: %v", err)
	}
	return client
}

func (m *timedModel) roundTrip(r *http.Request) (*http.Response, error) {
	reply := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}
	}
	if strings.Contains(r.URL.Path, "cachedContents") {
		return reply(http.StatusOK, cachedContentReply), nil
	}

	m.mu.Lock()
	n := len(m.calls)
	m.mu.Unlock()
	if n >= len(m.script) {
		m.t.Errorf("model call %d: got an unscripted call, want at most %d", n+1, len(m.script))
		return reply(http.StatusBadRequest, `{"error":{"code":400,"message":"unscripted call","status":"INVALID_ARGUMENT"}}`), nil
	}
	call := m.script[n]
	start := time.Now()
	time.Sleep(call.delay)
	m.mu.Lock()
	m.calls = append(m.calls, callWindow{start: start, end: time.Now()})
	m.mu.Unlock()
	if call.unavailable {
		return reply(http.StatusServiceUnavailable, `{"error":{"code":503,"message":"upstream unavailable","status":"UNAVAILABLE"}}`), nil
	}
	return reply(http.StatusOK, call.body), nil
}

func (m *timedModel) windows() []callWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]callWindow(nil), m.calls...)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// slowToolTurn is a model response calling slow_tool. n sets distinct token
// counts so the tests can tell which response landed on which turn.
func slowToolTurn(id string, n int) string {
	return fmt.Sprintf(`{
	"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":%q,"name":"slow_tool","args":{}}}]}}],
	"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}
}`, id, 100*n, n, 101*n)
}

// answerTurn is a model response with a final JSON answer.
func answerTurn(n int) string {
	return fmt.Sprintf(`{
	"candidates":[{"content":{"role":"model","parts":[{"text":"{\"answer\":\"done\"}"}]}}],
	"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}
}`, 100*n, n, 101*n)
}

// slowTool records its call on the trace, as the googletool helpers do, and
// takes toolDelay to run.
var slowTool = map[string]googletool.Metadata[errCapResponse]{
	"slow_tool": {
		Definition: &genai.FunctionDeclaration{Name: "slow_tool"},
		Handler: func(_ context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
			tc := trace.StartToolCall(call.ID, call.Name, call.Args)
			time.Sleep(toolDelay)
			tc.Complete(map[string]any{"ok": true}, nil)
			return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"ok": true}}
		},
	},
}

// turnShape is the part of a recorded turn the boundary tests pin.
type turnShape struct {
	Index    int
	Duration time.Duration
	// ModelTime runs from the turn's start to its first tool call's start.
	// It is zero for a turn without tool calls.
	ModelTime           time.Duration
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	Errors              int
	Failed              bool
	// ModelCalls are the 1-based numbers of the model calls that ran inside
	// the turn's window.
	ModelCalls []int
	// ToolCalls are the IDs of the tool calls that ran inside the turn's
	// window.
	ToolCalls []string
}

// turnShapes summarizes each recorded turn of trace, given the model calls
// the timed model answered.
func turnShapes(trace *agenttrace.Trace[errCapResponse], calls []callWindow) []turnShape {
	shapes := make([]turnShape, 0, len(trace.Turns))
	for _, turn := range trace.Turns {
		inside := func(start, end time.Time) bool {
			return !start.Before(turn.StartTime) && !end.After(turn.EndTime)
		}
		shape := turnShape{
			Index:               turn.Index,
			Duration:            turn.EndTime.Sub(turn.StartTime),
			InputTokens:         turn.InputTokens,
			OutputTokens:        turn.OutputTokens,
			CacheCreationTokens: turn.CacheCreationTokens,
			Errors:              len(turn.Errors),
			Failed:              turn.Failed,
		}
		for i, call := range calls {
			if inside(call.start, call.end) {
				shape.ModelCalls = append(shape.ModelCalls, i+1)
			}
		}
		for _, tc := range trace.ToolCalls {
			if !inside(tc.StartTime, tc.EndTime) {
				continue
			}
			if d := tc.StartTime.Sub(turn.StartTime); len(shape.ToolCalls) == 0 || d < shape.ModelTime {
				shape.ModelTime = d
			}
			shape.ToolCalls = append(shape.ToolCalls, tc.ID)
		}
		shapes = append(shapes, shape)
	}
	return shapes
}

// TestTurnWrapsItsModelCallAndToolRuns pins the turn boundary: turn N begins
// before the model call that produces response N, records that response's
// tokens, runs the tools the response requested, and ends before the next
// call. The time from a turn's start to its first tool call is the model
// call's duration, and each tool span is a child of the turn that requested
// it.
func TestTurnWrapsItsModelCallAndToolRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		prev := otel.GetTracerProvider()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
		t.Cleanup(func() { otel.SetTracerProvider(prev) })

		// Each call takes a different time, so a call timed on the wrong
		// turn changes that turn's duration.
		model := &timedModel{t: t, script: []scriptedCall{
			{delay: 30 * time.Millisecond, body: slowToolTurn("call_1", 1)},
			{delay: 50 * time.Millisecond, body: slowToolTurn("call_2", 2)},
			{delay: 70 * time.Millisecond, body: answerTurn(3)},
		}}
		prompt, err := promptbuilder.NewPrompt("hello")
		if err != nil {
			t.Fatalf("NewPrompt: %v", err)
		}
		// Context caching stays on: its creation tokens belong to turn 0.
		exec, err := googleexecutor.New[errCapRequest, errCapResponse](model.client(), prompt,
			googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		tracer := &recordingTracer{}
		got, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, slowTool)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got.Answer != "done" {
			t.Errorf("Execute answer: got = %q, want = %q", got.Answer, "done")
		}
		if len(tracer.traces) != 1 {
			t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
		}

		want := []turnShape{{
			Index: 0, Duration: 30*time.Millisecond + toolDelay, ModelTime: 30 * time.Millisecond,
			InputTokens: 100, OutputTokens: 1, CacheCreationTokens: 5,
			ModelCalls: []int{1}, ToolCalls: []string{"call_1"},
		}, {
			Index: 1, Duration: 50*time.Millisecond + toolDelay, ModelTime: 50 * time.Millisecond,
			InputTokens: 200, OutputTokens: 2,
			ModelCalls: []int{2}, ToolCalls: []string{"call_2"},
		}, {
			Index: 2, Duration: 70 * time.Millisecond,
			InputTokens: 300, OutputTokens: 3,
			ModelCalls: []int{3},
		}}
		if diff := cmp.Diff(want, turnShapes(tracer.traces[0], model.windows())); diff != "" {
			t.Errorf("turn shapes (-want +got):\n%s", diff)
		}

		wantParents := map[string]int64{"call_1": 0, "call_2": 1}
		if diff := cmp.Diff(wantParents, toolSpanTurns(t, spans.Ended())); diff != "" {
			t.Errorf("turn index of each tool span's parent (-want +got):\n%s", diff)
		}
	})
}

// toolSpanTurns maps each execute_tool span's tool.id to the turn index of
// its parent chat span, and reports a tool span whose window is not inside
// its parent's.
func toolSpanTurns(t *testing.T, spans []sdktrace.ReadOnlySpan) map[string]int64 {
	t.Helper()
	attr := func(s sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
		for _, kv := range s.Attributes() {
			if kv.Key == key {
				return kv.Value
			}
		}
		return attribute.Value{}
	}
	chats := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		if strings.HasPrefix(s.Name(), "chat ") {
			chats[s.SpanContext().SpanID().String()] = s
		}
	}
	got := make(map[string]int64, len(spans))
	for _, s := range spans {
		if !strings.HasPrefix(s.Name(), "execute_tool ") {
			continue
		}
		id := attr(s, "tool.id")
		parent, ok := chats[s.Parent().SpanID().String()]
		if !ok {
			t.Errorf("tool span %q: got a parent that is not a chat span, want its turn's chat span", id.AsString())
			continue
		}
		if s.StartTime().Before(parent.StartTime()) || s.EndTime().After(parent.EndTime()) {
			t.Errorf("tool span %q: got window [%v, %v], want it inside its turn's [%v, %v]",
				id.AsString(), s.StartTime(), s.EndTime(), parent.StartTime(), parent.EndTime())
		}
		got[id.AsString()] = attr(parent, "driftlessaf.turn.index").AsInt64()
	}
	return got
}

// TestRetryCallsLandOnTheNextTurn pins that every request a turn sends in
// reply to its response, other than the tool responses covered above, is the
// next turn's model call: the turn that decides to retry ends before the
// retry starts, and the retry's response tokens land on the turn that wraps
// it.
func TestRetryCallsLandOnTheNextTurn(t *testing.T) {
	submitOpt := googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (googletool.SubmitMetadata[errCapResponse], error) {
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
	})
	usage := `"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":1,"totalTokenCount":101}`
	// The turn that decides to retry wraps only the first call.
	firstTurn := turnShape{Index: 0, Duration: 30 * time.Millisecond, InputTokens: 100, OutputTokens: 1, ModelCalls: []int{1}}
	secondTurn := turnShape{Index: 1, Duration: 50 * time.Millisecond, InputTokens: 200, OutputTokens: 2, ModelCalls: []int{2}}

	for _, tc := range []struct {
		name      string
		first     string
		second    string
		extraOpts []googleexecutor.Option[errCapRequest, errCapResponse]
		want      []turnShape
	}{{
		name:   "max tokens",
		first:  `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"role":"model","parts":[{"text":"cut off"}]}}],` + usage + `}`,
		second: answerTurn(2),
		want:   []turnShape{firstTurn, secondTurn},
	}, {
		name:   "malformed function call",
		first:  `{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}],` + usage + `}`,
		second: answerTurn(2),
		want:   []turnShape{firstTurn, secondTurn},
	}, {
		name:   "empty response",
		first:  `{"candidates":[{"content":{"role":"model","parts":[]}}],` + usage + `}`,
		second: answerTurn(2),
		want:   []turnShape{firstTurn, secondTurn},
	}, {
		name:      "submit redirect",
		first:     `{"candidates":[{"content":{"role":"model","parts":[{"text":"I am done."}]}}],` + usage + `}`,
		second:    `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_submit","name":"submit_result","args":{"answer":"done"}}}]}}],"usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":2,"totalTokenCount":202}}`,
		extraOpts: []googleexecutor.Option[errCapRequest, errCapResponse]{submitOpt},
		// The submit call is a tool call of the turn whose response made it.
		want: []turnShape{firstTurn, {
			Index: 1, Duration: 50 * time.Millisecond, ModelTime: 50 * time.Millisecond,
			InputTokens: 200, OutputTokens: 2,
			ModelCalls: []int{2}, ToolCalls: []string{"call_submit"},
		}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				model := &timedModel{t: t, script: []scriptedCall{
					{delay: 30 * time.Millisecond, body: tc.first},
					{delay: 50 * time.Millisecond, body: tc.second},
				}}
				prompt, err := promptbuilder.NewPrompt("hello")
				if err != nil {
					t.Fatalf("NewPrompt: %v", err)
				}
				opts := append([]googleexecutor.Option[errCapRequest, errCapResponse]{
					googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
					googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
				}, tc.extraOpts...)
				exec, err := googleexecutor.New[errCapRequest, errCapResponse](model.client(), prompt, opts...)
				if err != nil {
					t.Fatalf("New: %v", err)
				}

				tracer := &recordingTracer{}
				got, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil)
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if got.Answer != "done" {
					t.Errorf("Execute answer: got = %q, want = %q", got.Answer, "done")
				}
				if len(tracer.traces) != 1 {
					t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
				}

				if diff := cmp.Diff(tc.want, turnShapes(tracer.traces[0], model.windows())); diff != "" {
					t.Errorf("turn shapes (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestTransientErrorsLandOnTheTurnOfTheirCall pins that a transient error the
// retry recovers from is recorded on the turn that wraps the failing call,
// including the first call.
func TestTransientErrorsLandOnTheTurnOfTheirCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		model := &timedModel{t: t, script: []scriptedCall{
			{delay: 5 * time.Millisecond, unavailable: true},
			{delay: 30 * time.Millisecond, body: slowToolTurn("call_1", 1)},
			{delay: 5 * time.Millisecond, unavailable: true},
			{delay: 50 * time.Millisecond, body: answerTurn(2)},
		}}
		prompt, err := promptbuilder.NewPrompt("hello")
		if err != nil {
			t.Fatalf("NewPrompt: %v", err)
		}
		exec, err := googleexecutor.New[errCapRequest, errCapResponse](model.client(), prompt,
			googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(1)),
			googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		tracer := &recordingTracer{}
		if _, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, slowTool); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(tracer.traces) != 1 {
			t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
		}

		// Each turn's model time is the failed attempt, fastRetry's 1µs
		// backoff, and the attempt that answers.
		want := []turnShape{{
			Index: 0, Duration: 5*time.Millisecond + time.Microsecond + 30*time.Millisecond + toolDelay, ModelTime: 5*time.Millisecond + time.Microsecond + 30*time.Millisecond,
			InputTokens: 100, OutputTokens: 1, Errors: 1,
			ModelCalls: []int{1, 2}, ToolCalls: []string{"call_1"},
		}, {
			Index: 1, Duration: 5*time.Millisecond + time.Microsecond + 50*time.Millisecond,
			InputTokens: 200, OutputTokens: 2, Errors: 1,
			ModelCalls: []int{3, 4},
		}}
		if diff := cmp.Diff(want, turnShapes(tracer.traces[0], model.windows())); diff != "" {
			t.Errorf("turn shapes (-want +got):\n%s", diff)
		}
	})
}

// TestMaxTurnsSyntheticTurnWrapsTheLastCall pins the turn budget: after the
// last allowed turn, the executor still sends that turn's tool responses, on
// a synthetic turn that wraps the call and records the response's tokens
// without running the tools it requests. When that call fails, the synthetic
// turn fails and the executor returns the call's error, not ErrMaxTurns.
func TestMaxTurnsSyntheticTurnWrapsTheLastCall(t *testing.T) {
	firstTurn := turnShape{
		Index: 0, Duration: 30*time.Millisecond + toolDelay, ModelTime: 30 * time.Millisecond,
		InputTokens: 100, OutputTokens: 1,
		ModelCalls: []int{1}, ToolCalls: []string{"call_1"},
	}
	for _, tc := range []struct {
		name         string
		last         scriptedCall
		wantMaxTurns bool
		// wantAPICode is the provider status code the returned error
		// carries; zero means it carries none.
		wantAPICode int
		want        []turnShape
	}{{
		name:         "last call answers",
		last:         scriptedCall{delay: 50 * time.Millisecond, body: slowToolTurn("call_2", 2)},
		wantMaxTurns: true,
		want: []turnShape{firstTurn, {
			Index: 1, Duration: 50 * time.Millisecond,
			InputTokens: 200, OutputTokens: 2,
			ModelCalls: []int{2},
		}},
	}, {
		name:        "last call fails",
		last:        scriptedCall{delay: 50 * time.Millisecond, unavailable: true},
		wantAPICode: http.StatusServiceUnavailable,
		want: []turnShape{firstTurn, {
			Index: 1, Duration: 50 * time.Millisecond, Errors: 1, Failed: true,
			ModelCalls: []int{2},
		}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				model := &timedModel{t: t, script: []scriptedCall{
					{delay: 30 * time.Millisecond, body: slowToolTurn("call_1", 1)},
					tc.last,
				}}
				prompt, err := promptbuilder.NewPrompt("hello")
				if err != nil {
					t.Fatalf("NewPrompt: %v", err)
				}
				exec, err := googleexecutor.New[errCapRequest, errCapResponse](model.client(), prompt,
					googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
					googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
					googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
				)
				if err != nil {
					t.Fatalf("New: %v", err)
				}

				tracer := &recordingTracer{}
				_, err = exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, slowTool)
				if err == nil {
					t.Fatal("Execute: got nil error, want non-nil")
				}
				if got := errors.Is(err, agentexecutor.ErrMaxTurns); got != tc.wantMaxTurns {
					t.Errorf("errors.Is(%v, ErrMaxTurns): got = %t, want = %t", err, got, tc.wantMaxTurns)
				}
				apiErr, _ := errors.AsType[genai.APIError](err)
				if apiErr.Code != tc.wantAPICode {
					t.Errorf("provider status code in %v: got = %d, want = %d", err, apiErr.Code, tc.wantAPICode)
				}
				if len(tracer.traces) != 1 {
					t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
				}
				if diff := cmp.Diff(tc.want, turnShapes(tracer.traces[0], model.windows())); diff != "" {
					t.Errorf("turn shapes (-want +got):\n%s", diff)
				}
			})
		})
	}
}
