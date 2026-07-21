/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
)

// TestAppendUserPromptSuffix asserts the exact concatenation the executors
// perform on the built user prompt: nil suffix passes the prompt through
// unchanged, a bound suffix is appended with a blank-line separator, and an
// unbuildable suffix (unbound placeholder) surfaces as an error.
func TestAppendUserPromptSuffix(t *testing.T) {
	t.Parallel()

	suffix, err := promptbuilder.NewPrompt("lens suffix body")
	if err != nil {
		t.Fatalf("NewPrompt(suffix) error = %v", err)
	}
	unbuildable, err := promptbuilder.NewPrompt("{{unbound}}")
	if err != nil {
		t.Fatalf("NewPrompt(unbuildable) error = %v", err)
	}

	tests := []struct {
		name    string
		suffix  *promptbuilder.Prompt
		want    string
		wantErr bool
	}{
		{name: "nil suffix passes prompt through", suffix: nil, want: "changeset payload"},
		{name: "suffix appended with blank-line separator", suffix: suffix, want: "changeset payload\n\nlens suffix body"},
		{name: "unbuildable suffix returns error", suffix: unbuildable, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := AppendUserPromptSuffix("changeset payload", tt.suffix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AppendUserPromptSuffix() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Errorf("AppendUserPromptSuffix(): got = %q, want = %q", got, tt.want)
			}
		})
	}
}

// TestDefaultResourceLabels pins the environment-derived default behavior the
// executors share: values come from the deployment env vars, unset vars fall
// back to "unknown", and custom labels override defaults on key match while
// extra keys pass through. No t.Parallel: t.Setenv forbids it.
func TestDefaultResourceLabels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
		job     string
		product string
		team    string
		labels  map[string]string
		want    map[string]string
	}{
		{
			name: "all env unset falls back to unknown",
			want: map[string]string{"service_name": "unknown", "product": "unknown", "team": "unknown"},
		},
		{
			name:    "defaults derived from environment",
			service: "skillup-rec",
			product: "agents",
			team:    "dev-platform",
			want:    map[string]string{"service_name": "skillup-rec", "product": "agents", "team": "dev-platform"},
		},
		{
			name: "service name falls back to CLOUD_RUN_JOB",
			job:  "requeue-cron",
			want: map[string]string{"service_name": "requeue-cron", "product": "unknown", "team": "unknown"},
		},
		{
			name:    "custom labels override defaults and add keys",
			service: "skillup-rec",
			product: "agents",
			team:    "dev-platform",
			labels:  map[string]string{"team": "platform", "model": "claude"},
			want:    map[string]string{"service_name": "skillup-rec", "product": "agents", "team": "platform", "model": "claude"},
		},
		{
			name:   "custom labels merge over unknown defaults",
			labels: map[string]string{"product": "agents"},
			want:   map[string]string{"service_name": "unknown", "product": "agents", "team": "unknown"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("K_SERVICE", tc.service)
			t.Setenv("CLOUD_RUN_JOB", tc.job)
			t.Setenv("CHAINGUARD_PRODUCT", tc.product)
			t.Setenv("CHAINGUARD_TEAM", tc.team)

			got := DefaultResourceLabels(tc.labels)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("DefaultResourceLabels() mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestSubmitPredicate pins the single submit-routing rule the executors
// share: a call routes to the terminal submit tool only when its name is not
// registered as a regular tool, matches the submit tool's name, and a submit
// handler is configured.
func TestSubmitPredicate(t *testing.T) {
	t.Parallel()

	tools := map[string]struct{}{"read_file": {}, "submit_result": {}}
	tests := []struct {
		name             string
		submitToolName   string
		submitConfigured bool
		call             string
		want             bool
	}{
		{name: "submit name unregistered routes to submit", submitToolName: "finish", submitConfigured: true, call: "finish", want: true},
		{name: "registered tool takes precedence over submit name", submitToolName: "submit_result", submitConfigured: true, call: "submit_result", want: false},
		{name: "no submit handler configured", submitToolName: "finish", submitConfigured: false, call: "finish", want: false},
		{name: "unrelated name does not route to submit", submitToolName: "finish", submitConfigured: true, call: "other", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			isSubmit := SubmitPredicate(tools, tt.submitToolName, tt.submitConfigured)
			if got := isSubmit(tt.call); got != tt.want {
				t.Errorf("isSubmit(%q): got = %v, want = %v", tt.call, got, tt.want)
			}
		})
	}
}

// TestDispatchToolCallsHoldsSubmitOutOfPool asserts the submit-quiesce
// contract: every pooled handler finishes before any submit call runs, and
// each run lands in its original slot regardless of dispatch order.
func TestDispatchToolCallsHoldsSubmitOutOfPool(t *testing.T) {
	t.Parallel()

	calls := []string{"submit_result", "read_file", "list_dir", "submit_result"}
	isSubmit := func(call string) bool { return call == "submit_result" }

	var pooled atomic.Int32
	pooledDoneAtSubmit := make([]int32, 0, 2)
	slots := make([]string, len(calls))
	DispatchToolCalls(calls, 4, isSubmit, func(i int, call string) {
		slots[i] = call
		if isSubmit(call) {
			pooledDoneAtSubmit = append(pooledDoneAtSubmit, pooled.Load())
			return
		}
		pooled.Add(1)
	})

	if diff := cmp.Diff(calls, slots); diff != "" {
		t.Errorf("slot order mismatch (-want, +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int32{2, 2}, pooledDoneAtSubmit); diff != "" {
		t.Errorf("pooled handlers finished before each submit run (-want, +got):\n%s", diff)
	}
}

// TestDispatchToolCallsBoundsConcurrency asserts that no more than the
// configured number of pooled handlers run at once, and that a non-positive
// concurrency clamps to strictly sequential dispatch.
func TestDispatchToolCallsBoundsConcurrency(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		concurrency int
		wantMax     int
	}{
		{name: "bounded pool", concurrency: 2, wantMax: 2},
		{name: "non-positive clamps to sequential", concurrency: 0, wantMax: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			inFlight, maxInFlight := 0, 0
			DispatchToolCalls(make([]int, 8), tt.concurrency, func(int) bool { return false }, func(int, int) {
				mu.Lock()
				inFlight++
				maxInFlight = max(maxInFlight, inFlight)
				mu.Unlock()

				mu.Lock()
				inFlight--
				mu.Unlock()
			})
			if maxInFlight > tt.wantMax {
				t.Errorf("max in-flight handlers: got = %d, want <= %d", maxInFlight, tt.wantMax)
			}
		})
	}
}

// TestGateSubmissionRejectionKinds pins the trace-record kinds GateSubmission
// produces: a validator-findings rejection returns the findings to the model
// and records a recoverable tool call (the model corrects and resubmits),
// while a validator error aborts the run and records a genuine failure.
func TestGateSubmissionRejectionKinds(t *testing.T) {
	type reviewResult struct {
		Verdict string `json:"verdict"`
	}
	rec := telemetry.NewRecorder(metrics.NewGenAI("test"), "model", "provider", nil, nil)
	outcome := toolcall.SubmitOutcome[reviewResult]{
		Accepted:   true,
		Response:   reviewResult{Verdict: "pass"},
		Reasoning:  "all checks passed",
		ToolResult: map[string]any{"success": true},
	}

	t.Run("validator findings record a recoverable rejection", func(t *testing.T) {
		ctx := t.Context()
		trace, _ := agenttrace.StartTrace[reviewResult](ctx, "prompt")
		findingsValidator := func(context.Context, reviewResult, string) ([]callbacks.Finding, error) {
			return []callbacks.Finding{{Details: "verdict lacks evidence"}}, nil
		}

		var result reviewResult
		toolResult, committed, err := GateSubmission(ctx, outcome, trace, "call-1", "submit_result",
			map[string]any{"verdict": "pass"},
			[]callbacks.ResultValidator[reviewResult]{findingsValidator}, rec, "submit_result", &result)
		if err != nil {
			t.Fatalf("GateSubmission() error = %v, want = nil (findings reject, they do not abort)", err)
		}
		if committed {
			t.Errorf("committed: got = true, want = false")
		}
		if toolResult == nil {
			t.Errorf("tool result: got = nil, want = rejection payload for the model")
		}
		if len(trace.ToolCalls) != 1 {
			t.Fatalf("tool calls length: got = %d, want = 1", len(trace.ToolCalls))
		}
		if tc := trace.ToolCalls[0]; tc.Error == nil || !tc.Recoverable {
			t.Errorf("tool call record: got = {error: %v, recoverable: %t}, want = rejection error with recoverable = true", tc.Error, tc.Recoverable)
		}
	})

	t.Run("validator error records a genuine failure", func(t *testing.T) {
		ctx := t.Context()
		trace, _ := agenttrace.StartTrace[reviewResult](ctx, "prompt")
		errValidator := func(context.Context, reviewResult, string) ([]callbacks.Finding, error) {
			return nil, errors.New("validator infrastructure down")
		}

		var result reviewResult
		_, committed, err := GateSubmission(ctx, outcome, trace, "call-1", "submit_result",
			map[string]any{"verdict": "pass"},
			[]callbacks.ResultValidator[reviewResult]{errValidator}, rec, "submit_result", &result)
		if err == nil {
			t.Fatalf("GateSubmission() error = nil, want = validator error (aborts the run)")
		}
		if committed {
			t.Errorf("committed: got = true, want = false")
		}
		if len(trace.ToolCalls) != 1 {
			t.Fatalf("tool calls length: got = %d, want = 1", len(trace.ToolCalls))
		}
		if tc := trace.ToolCalls[0]; tc.Error == nil || tc.Recoverable {
			t.Errorf("tool call record: got = {error: %v, recoverable: %t}, want = terminal error with recoverable = false", tc.Error, tc.Recoverable)
		}
	})
}
