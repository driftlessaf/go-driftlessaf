/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

const deadlineNudge = "The time allowed for this run is almost over."

// deadlineRunBudget is how far the context deadline is from the fake clock's
// start in every deadline test.
const deadlineRunBudget = 360 * time.Second

// fakeClock is the executor's clock in the deadline tests. The run context
// carries a real deadline deadlineRunBudget away, which no test reaches; the
// executor compares that deadline with this clock, which only advance moves.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// deadlineRun drives one execution against a scripted model. Each scripted
// turn is "lookup" (a tool call whose handler takes lookupTakes on the fake
// clock) or a submit_result answer; the validator rejects the answer
// "ungrounded".
type deadlineRun struct {
	opts        []claudeexecutor.Option[errCapRequest, errCapResponse]
	script      []string
	lookupTakes time.Duration
	// noDeadline runs under a context with no deadline.
	noDeadline bool
}

func (r deadlineRun) execute(t *testing.T) (errCapResponse, [][]byte, *agenttrace.Trace[errCapResponse], error) {
	t.Helper()
	client, requests := newExecTestClient(t, func(n int) []string {
		if n > len(r.script) {
			t.Errorf("request %d: the script has %d turns", n, len(r.script))
			return promptSizedToolTurn(t, n, "submit_result", submitInput("unscripted"), 10, 0, 0)
		}
		if step := r.script[n-1]; step != "lookup" {
			return promptSizedToolTurn(t, n, "submit_result", submitInput(step), 10, 0, 0)
		}
		return promptSizedToolTurn(t, n, "lookup", map[string]any{"q": "x"}, 10, 0, 0)
	})

	clock := &fakeClock{now: time.Now()}
	ctx := t.Context()
	if !r.noDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, clock.Now().Add(deadlineRunBudget))
		defer cancel()
	}
	tracer := &recordingTracer{}
	ctx = agenttrace.WithTracer[errCapResponse](ctx, tracer)

	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, promptbuilder.MustNewPrompt("Search the repository."),
		append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
			claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](8),
			claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
			claudeexecutor.WithClock[errCapRequest, errCapResponse](clock.Now),
			claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
			claudeexecutor.WithResultValidator[errCapRequest, errCapResponse](func(_ context.Context, resp errCapResponse, _ string) ([]callbacks.Finding, error) {
				if resp.Answer == "ungrounded" {
					return []callbacks.Finding{{Kind: callbacks.FindingKindReview, Identifier: "ungrounded", Details: "the answer cites nothing"}}, nil
				}
				return nil, nil
			}),
		}, r.opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	result, err := exec.Execute(ctx, errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				clock.advance(r.lookupTakes)
				return map[string]any{"result": "evidence"}
			},
		},
	})
	if len(tracer.traces) != 1 {
		t.Fatalf("traces: got = %d, want = 1", len(tracer.traces))
	}
	return result, requests(), tracer.traces[0], err
}

func finalizeBeforeDeadline(d time.Duration) claudeexecutor.Option[errCapRequest, errCapResponse] {
	return claudeexecutor.WithFinalizeBeforeDeadline[errCapRequest, errCapResponse](d)
}

func minRequestBudget(d time.Duration) claudeexecutor.Option[errCapRequest, errCapResponse] {
	return claudeexecutor.WithMinRequestBudget[errCapRequest, errCapResponse](d)
}

// TestFinalizeBeforeDeadline pins when the deadline nudge reaches the model
// and what follows it. Each lookup takes 100s of a 360s run, so the third
// lookup ends with 60s left.
func TestFinalizeBeforeDeadline(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       []claudeexecutor.Option[errCapRequest, errCapResponse]
		noDeadline bool
		script     []string
		// wantNudgedRequest is the 1-based request that first carries the
		// nudge and forces the submit tool; zero means no request does.
		wantNudgedRequest int
		wantAnswer        string
	}{{
		name:              "nudges on the first tool turn that ends inside the threshold",
		opts:              []claudeexecutor.Option[errCapRequest, errCapResponse]{finalizeBeforeDeadline(110 * time.Second)},
		script:            []string{"lookup", "lookup", "lookup", "grounded"},
		wantNudgedRequest: 4,
		wantAnswer:        "grounded",
	}, {
		name:              "a rejected submission and a further lookup draw no second nudge",
		opts:              []claudeexecutor.Option[errCapRequest, errCapResponse]{finalizeBeforeDeadline(110 * time.Second)},
		script:            []string{"lookup", "lookup", "lookup", "ungrounded", "lookup", "grounded"},
		wantNudgedRequest: 4,
		wantAnswer:        "grounded",
	}, {
		name:       "a threshold the run never comes inside sends nothing",
		opts:       []claudeexecutor.Option[errCapRequest, errCapResponse]{finalizeBeforeDeadline(50 * time.Second)},
		script:     []string{"lookup", "lookup", "lookup", "grounded"},
		wantAnswer: "grounded",
	}, {
		name:       "zero is off",
		opts:       []claudeexecutor.Option[errCapRequest, errCapResponse]{finalizeBeforeDeadline(0)},
		script:     []string{"lookup", "lookup", "lookup", "grounded"},
		wantAnswer: "grounded",
	}, {
		name:       "a context with no deadline sends nothing",
		opts:       []claudeexecutor.Option[errCapRequest, errCapResponse]{finalizeBeforeDeadline(110 * time.Second)},
		noDeadline: true,
		script:     []string{"lookup", "lookup", "lookup", "grounded"},
		wantAnswer: "grounded",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			result, reqs, trace, err := deadlineRun{opts: tc.opts, script: tc.script, lookupTakes: 100 * time.Second, noDeadline: tc.noDeadline}.execute(t)
			if err != nil || result.Answer != tc.wantAnswer {
				t.Fatalf("Execute: result = %+v, err = %v, want answer %q", result, err, tc.wantAnswer)
			}
			if len(reqs) != len(tc.script) {
				t.Fatalf("requests: got = %d, want = %d", len(reqs), len(tc.script))
			}
			for i, body := range reqs {
				n := i + 1
				wantNudges := 0
				if tc.wantNudgedRequest != 0 && n >= tc.wantNudgedRequest {
					wantNudges = 1
				}
				if got := strings.Count(string(body), deadlineNudge); got != wantNudges {
					t.Errorf("request %d: deadline nudges in the conversation: got = %d, want = %d", n, got, wantNudges)
				}
				if got, want := forcesSubmit(t, string(body)), n == tc.wantNudgedRequest; got != want {
					t.Errorf("request %d forces submit_result: got = %v, want = %v", n, got, want)
				}
			}
			_, nudged := trace.Metadata[agentexecutor.MetadataDeadlineFinalizeNudge]
			if want := tc.wantNudgedRequest != 0; nudged != want {
				t.Errorf("trace metadata records the nudge: got = %v, want = %v", nudged, want)
			}
		})
	}
}

// TestDeadlineNudgeRecordedOnlyWhenSent pins that the trace records the nudge
// only once a request carrying it has gone to the model. Each lookup takes
// 89s of a 360s run, so the fourth lookup is the first to end inside a 50s
// threshold, with 4s left; the nudge is appended there and the run then ends
// before the next request.
func TestDeadlineNudgeRecordedOnlyWhenSent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []claudeexecutor.Option[errCapRequest, errCapResponse]
		wantErr error
	}{{
		name:    "the minimum request budget ends the run before the nudged request",
		opts:    []claudeexecutor.Option[errCapRequest, errCapResponse]{finalizeBeforeDeadline(50 * time.Second), minRequestBudget(10 * time.Second)},
		wantErr: agentexecutor.ErrDeadlineBudget,
	}, {
		name: "the nudge is appended on the last allowed turn",
		opts: []claudeexecutor.Option[errCapRequest, errCapResponse]{
			finalizeBeforeDeadline(50 * time.Second),
			claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](4),
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, reqs, trace, err := deadlineRun{
				opts:        tc.opts,
				script:      []string{"lookup", "lookup", "lookup", "lookup", "grounded"},
				lookupTakes: 89 * time.Second,
			}.execute(t)
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("Execute: err = %v, want an error matching %v", err, tc.wantErr)
			}
			if len(reqs) != 4 {
				t.Fatalf("requests: got = %d, want = 4", len(reqs))
			}
			for i, body := range reqs {
				if strings.Contains(string(body), deadlineNudge) {
					t.Errorf("request %d carries the deadline nudge", i+1)
				}
			}
			if _, nudged := trace.Metadata[agentexecutor.MetadataDeadlineFinalizeNudge]; nudged {
				t.Error("trace metadata records a nudge no request carried")
			}
		})
	}
}

// TestDeadlineOptionsOffLeaveRequestsUnchanged pins that zero-valued deadline
// options send byte-identical requests to an executor built without them,
// under a deadline both features would otherwise act on.
func TestDeadlineOptionsOffLeaveRequestsUnchanged(t *testing.T) {
	run := deadlineRun{script: []string{"lookup", "lookup", "lookup", "lookup", "grounded"}, lookupTakes: 89 * time.Second}
	_, want, _, err := run.execute(t)
	if err != nil {
		t.Fatalf("Execute without the options: %v", err)
	}
	run.opts = []claudeexecutor.Option[errCapRequest, errCapResponse]{finalizeBeforeDeadline(0), minRequestBudget(0)}
	_, got, _, err := run.execute(t)
	if err != nil {
		t.Fatalf("Execute with zero options: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("requests: got = %d, want = %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("request %d differs with zero options:\n got = %s\nwant = %s", i+1, got[i], want[i])
		}
	}
}

// TestMinRequestBudget pins that no request starts with less than the minimum
// left before the deadline. Each lookup takes 89s of a 360s run, so the turns
// start with 360s, 271s, 182s, 93s and 4s left.
func TestMinRequestBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		min          time.Duration
		wantRequests int
		wantErr      error
	}{
		{name: "a turn with less than the minimum left is not started", min: 10 * time.Second, wantRequests: 4, wantErr: agentexecutor.ErrDeadlineBudget},
		{name: "a turn with the minimum left is started", min: 4 * time.Second, wantRequests: 5},
		{name: "zero is off", min: 0, wantRequests: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, reqs, _, err := deadlineRun{
				opts:        []claudeexecutor.Option[errCapRequest, errCapResponse]{minRequestBudget(tc.min)},
				script:      []string{"lookup", "lookup", "lookup", "lookup", "grounded"},
				lookupTakes: 89 * time.Second,
			}.execute(t)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Execute: err = %v, want = %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && result.Answer != "grounded" {
				t.Errorf("Execute: answer = %q, want = %q", result.Answer, "grounded")
			}
			if len(reqs) != tc.wantRequests {
				t.Errorf("requests: got = %d, want = %d", len(reqs), tc.wantRequests)
			}
		})
	}
}

// TestMinRequestBudgetBeforeTheFirstTurn pins that a run handed less than the
// minimum makes no request at all.
func TestMinRequestBudgetBeforeTheFirstTurn(t *testing.T) {
	_, reqs, _, err := deadlineRun{
		opts:   []claudeexecutor.Option[errCapRequest, errCapResponse]{minRequestBudget(deadlineRunBudget + time.Second)},
		script: []string{"grounded"},
	}.execute(t)
	if !errors.Is(err, agentexecutor.ErrDeadlineBudget) {
		t.Fatalf("Execute: err = %v, want = %v", err, agentexecutor.ErrDeadlineBudget)
	}
	if len(reqs) != 0 {
		t.Errorf("requests: got = %d, want = 0", len(reqs))
	}
}

func TestDeadlineOptionsRejectNegative(t *testing.T) {
	for name, opt := range map[string]claudeexecutor.Option[errCapRequest, errCapResponse]{
		"finalize before deadline": finalizeBeforeDeadline(-time.Second),
		"min request budget":       minRequestBudget(-time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := claudeexecutor.New[errCapRequest, errCapResponse](anthropic.Client{}, promptbuilder.MustNewPrompt("hello"), opt); err == nil {
				t.Error("New: got nil error, want a non-negative error")
			}
		})
	}
}
