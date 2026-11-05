/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/effort"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

const askTool = "ask_a_friend"

func suspendConfig(t *testing.T) Config[answer] {
	t.Helper()
	cfg := config(t)
	cfg.SuspendTool = &responses.FunctionToolParam{
		Name:        askTool,
		Description: param.NewOpt("Ask a human."),
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"question": map[string]any{"type": "string"}}, "required": []string{"question"}},
		Strict:      param.NewOpt(false),
	}
	return cfg
}

func ask(id, question string) map[string]any {
	b, _ := json.Marshal(map[string]any{"question": question})
	return call(id, askTool, string(b))
}

// recorder is a fake Responses endpoint that records each request body and
// answers with the next scripted turn. A request past the script fails the
// test.
type recorder struct {
	t      *testing.T
	mu     sync.Mutex
	script [][]map[string]any
	bodies []map[string]any
}

func (r *recorder) handle(w http.ResponseWriter, req *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		r.t.Error(err)
	}
	r.mu.Lock()
	n := len(r.bodies)
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	if n >= len(r.script) {
		r.t.Errorf("unexpected request %d", n)
		http.Error(w, "unscripted", http.StatusBadRequest)
		return
	}
	emit(w, r.script[n]...)
}

func (r *recorder) requests() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies
}

func newRecorder(t *testing.T, script ...[]map[string]any) *recorder {
	return &recorder{t: t, script: script}
}

// lookupTool counts its calls and answers with a value only it supplies.
func lookupTool(calls *atomic.Int32) toolcall.Tool[answer] {
	return toolcall.Tool[answer]{
		Def: toolcall.Definition{Name: "lookup", Description: "Look up a fixture."},
		Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[answer], _ *answer) map[string]any {
			calls.Add(1)
			return map[string]any{"value": "looked-up-" + call.ID}
		},
	}
}

func asResumer(t *testing.T, e Interface[request, answer]) interface {
	Resumer[request, answer]
	ResultResumer[request, answer]
} {
	t.Helper()
	r, ok := e.(interface {
		Resumer[request, answer]
		ResultResumer[request, answer]
	})
	if !ok {
		t.Fatal("executor does not implement Resumer and ResultResumer")
	}
	return r
}

// roundTrip passes the envelope through JSON, as a checkpoint store does.
func roundTrip(t *testing.T, env checkpoint.Envelope) checkpoint.Envelope {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var out checkpoint.Envelope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func inputItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["input"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		items = append(items, r.(map[string]any))
	}
	return items
}

// TestSuspendResumeContinuesTheSameConversation parks on a turn of
// [lookup, ask, lookup] behind encrypted reasoning, round-trips the envelope
// through JSON, and resumes on a fresh executor. The resumed request must be
// the parked conversation verbatim plus the turn's outputs in call order.
func TestSuspendResumeContinuesTheSameConversation(t *testing.T) {
	t.Parallel()
	secret := "opaque-" + rand.Text()
	question := "Which branch? " + rand.Text()
	answerText := "main " + rand.Text()
	turn0 := []map[string]any{
		{"id": "rs_1", "type": "reasoning", "summary": []any{}, "encrypted_content": secret},
		call("look-a", "lookup", `{"reasoning":"first"}`),
		ask("ask-b", question),
		call("look-c", "lookup", `{"reasoning":"second"}`),
	}
	parked := newRecorder(t, turn0)
	var lookups atomic.Int32
	tools := map[string]toolcall.Tool[answer]{"lookup": lookupTool(&lookups)}

	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got = %v, want a suspension", err)
	}
	// Both siblings finished before the suspension was returned.
	if got := lookups.Load(); got != 2 {
		t.Fatalf("lookups before suspension: got = %d, want = 2", got)
	}
	env := roundTrip(t, susp.Envelope)
	if err := env.Validate(); err != nil {
		t.Fatalf("envelope does not validate: %v", err)
	}
	if env.Provider != checkpoint.ProviderOpenAI || env.Model != "fixture-model" || env.Turn != 0 || env.RemainingTurns != 4 || env.Reason != checkpoint.ReasonAwaitingAnswer {
		t.Errorf("envelope header: got = %+v", env)
	}
	if want := []checkpoint.PendingToolCall{{ID: "ask-b", Name: askTool, InputJSON: json.RawMessage(ask("ask-b", question)["arguments"].(string))}}; !cmp.Equal(want, env.PendingToolCalls) {
		t.Errorf("pending calls (-want, +got):\n%s", cmp.Diff(want, env.PendingToolCalls))
	}
	if susp.Question != question {
		t.Errorf("question: got = %q, want = %q", susp.Question, question)
	}

	resumed := newRecorder(t, []map[string]any{submit("s", "done")})
	got, err := asResumer(t, serve(t, resumed.handle, suspendConfig(t))).Resume(t.Context(), env, map[string]string{"ask-b": answerText}, tools)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.Text != "done" {
		t.Errorf("result: got = %q, want = %q", got.Text, "done")
	}
	if got := lookups.Load(); got != 2 {
		t.Errorf("lookups after resume: got = %d, want = 2 (no rerun)", got)
	}

	first, next := parked.requests()[0], resumed.requests()[0]
	items := inputItems(t, next)
	if len(items) != 8 {
		t.Fatalf("resumed input items: got = %d, want = 8", len(items))
	}
	// The prompt replays exactly as the parked run sent it.
	if diff := cmp.Diff(inputItems(t, first), items[:1]); diff != "" {
		t.Errorf("prompt (-parked, +resumed):\n%s", diff)
	}
	if items[1]["type"] != "reasoning" || items[1]["encrypted_content"] != secret {
		t.Errorf("reasoning item: got = %v, want encrypted content preserved", items[1])
	}
	for i, id := range []string{"look-a", "ask-b", "look-c"} {
		if c := items[2+i]; c["type"] != "function_call" || c["call_id"] != id {
			t.Errorf("item %d: got = %v, want function_call %q", 2+i, c, id)
		}
		o := items[5+i]
		if o["type"] != "function_call_output" || o["call_id"] != id {
			t.Errorf("item %d: got = %v, want function_call_output %q", 5+i, o, id)
		}
	}
	if out := items[5]["output"].(string); !strings.Contains(out, "looked-up-look-a") {
		t.Errorf("first lookup output: got = %q", out)
	}
	if out := items[6]["output"].(string); !strings.Contains(out, answerText) {
		t.Errorf("answer output: got = %q, want it to carry the answer", out)
	}
	if out := items[7]["output"].(string); !strings.Contains(out, "looked-up-look-c") {
		t.Errorf("second lookup output: got = %q", out)
	}
	// The resumed request keeps the parked run's configuration.
	for _, key := range []string{"model", "tools", "tool_choice", "reasoning", "include", "store", "max_output_tokens"} {
		if diff := cmp.Diff(first[key], next[key]); diff != "" {
			t.Errorf("%s (-parked, +resumed):\n%s", key, diff)
		}
	}
}

// TestResuspendCarriesTheRunsOwnBudget parks on an executor with five turns,
// resumes on one with two hundred, and parks again: the second envelope
// carries what is left of the original five.
func TestResuspendCarriesTheRunsOwnBudget(t *testing.T) {
	t.Parallel()
	parked := newRecorder(t, []map[string]any{ask("q1", "first?")})
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, nil)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.RemainingTurns != 4 {
		t.Fatalf("Execute: got = %v, want a suspension with 4 turns left", err)
	}
	cfg := suspendConfig(t)
	cfg.MaxTurns = 200
	resumed := newRecorder(t, []map[string]any{ask("q2", "second?")})
	_, err = asResumer(t, serve(t, resumed.handle, cfg)).Resume(t.Context(), roundTrip(t, susp.Envelope), map[string]string{"q1": "yes"}, nil)
	again, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Resume: got = %v, want a second suspension", err)
	}
	if again.Turn != 1 || again.RemainingTurns != 3 {
		t.Errorf("second suspension: turn = %d, remaining = %d, want = 1, 3", again.Turn, again.RemainingTurns)
	}
}

// decodeState returns the envelope's providerState.
func decodeState(t *testing.T, env checkpoint.Envelope) providerState {
	t.Helper()
	var s providerState
	if err := json.Unmarshal(env.ProviderState, &s); err != nil {
		t.Fatalf("decode provider state: %v", err)
	}
	return s
}

// TestResuspendKeepsTheSavedConversation parks behind encrypted reasoning,
// resumes, parks again and resumes the second envelope. On a resumed run the
// saved items are param.Override values, so the second park re-encodes them:
// the second envelope must start with the first one's items byte for byte,
// and the last request must replay the resumed request's input unchanged.
func TestResuspendKeepsTheSavedConversation(t *testing.T) {
	t.Parallel()
	secret1, secret2 := "opaque-"+rand.Text(), "opaque-"+rand.Text()
	parked := newRecorder(t, []map[string]any{
		{"id": "rs_1", "type": "reasoning", "summary": []any{}, "encrypted_content": secret1},
		ask("q1", "first?"),
	})
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, nil)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got = %v, want a suspension", err)
	}
	env1 := roundTrip(t, susp.Envelope)

	resumed := newRecorder(t, []map[string]any{
		{"id": "rs_2", "type": "reasoning", "summary": []any{}, "encrypted_content": secret2},
		ask("q2", "second?"),
	})
	_, err = asResumer(t, serve(t, resumed.handle, suspendConfig(t))).Resume(t.Context(), env1, map[string]string{"q1": "yes"}, nil)
	again, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Resume: got = %v, want a second suspension", err)
	}
	env2 := roundTrip(t, again.Envelope)

	first, second := decodeState(t, env1), decodeState(t, env2)
	// The second envelope holds the first one's items, the q1 answer, and
	// the resumed turn's reasoning and call.
	if got, want := len(second.Input), len(first.Input)+3; got != want {
		t.Fatalf("second envelope input items: got = %d, want = %d", got, want)
	}
	for i, item := range first.Input {
		if string(second.Input[i]) != string(item) {
			t.Errorf("second envelope item %d: got = %s, want = %s", i, second.Input[i], item)
		}
	}

	final := newRecorder(t, []map[string]any{submit("s", "done")})
	if _, err := asResumer(t, serve(t, final.handle, suspendConfig(t))).Resume(t.Context(), env2, map[string]string{"q2": "no"}, nil); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	prev, last := inputItems(t, resumed.requests()[0]), inputItems(t, final.requests()[0])
	if len(last) != len(prev)+3 {
		t.Fatalf("final request input items: got = %d, want = %d", len(last), len(prev)+3)
	}
	if diff := cmp.Diff(prev, last[:len(prev)]); diff != "" {
		t.Errorf("replayed conversation (-resumed request, +final request):\n%s", diff)
	}
	for i, secret := range []string{secret1, secret2} {
		var found bool
		for _, item := range last {
			found = found || item["encrypted_content"] == secret
		}
		if !found {
			t.Errorf("final request: reasoning %d's encrypted content is missing", i+1)
		}
	}
}

// TestResumeCapsTheBudgetAtTheLiveMaxTurns parks with four turns left and
// resumes on an executor whose MaxTurns is one: the run ends with ErrMaxTurns
// after exactly one resumed request.
func TestResumeCapsTheBudgetAtTheLiveMaxTurns(t *testing.T) {
	t.Parallel()
	var lookups atomic.Int32
	tools := map[string]toolcall.Tool[answer]{"lookup": lookupTool(&lookups)}
	parked := newRecorder(t, []map[string]any{ask("q1", "first?")})
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.RemainingTurns != 4 {
		t.Fatalf("Execute: got = %v, want a suspension with 4 turns left", err)
	}
	cfg := suspendConfig(t)
	cfg.MaxTurns = 1
	resumed := newRecorder(t, []map[string]any{call("l1", "lookup", `{"reasoning":"check"}`)})
	_, err = asResumer(t, serve(t, resumed.handle, cfg)).Resume(t.Context(), roundTrip(t, susp.Envelope), map[string]string{"q1": "yes"}, tools)
	if !errors.Is(err, agentexecutor.ErrMaxTurns) {
		t.Fatalf("Resume: got = %v, want ErrMaxTurns", err)
	}
	if n := len(resumed.requests()); n != 1 {
		t.Errorf("resumed requests: got = %d, want = 1", n)
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("lookup calls: got = %d, want = 1", n)
	}
}

func TestResumeRejectsDrift(t *testing.T) {
	t.Parallel()
	parked := newRecorder(t, []map[string]any{ask("q", "why?")})
	var lookups atomic.Int32
	tools := map[string]toolcall.Tool[answer]{"lookup": lookupTool(&lookups)}
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got = %v, want a suspension", err)
	}
	env := roundTrip(t, susp.Envelope)
	other, err := promptbuilder.NewPrompt("Different instructions.")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config[answer])
		tools  map[string]toolcall.Tool[answer]
		drift  bool
	}{
		{name: "model", mutate: func(c *Config[answer]) { c.Model = "other-model" }, tools: tools, drift: true},
		{name: "system instructions", mutate: func(c *Config[answer]) { c.SystemInstructions = other }, tools: tools, drift: true},
		{name: "tool set", mutate: func(*Config[answer]) {}, tools: nil, drift: true},
		{name: "effort", mutate: func(c *Config[answer]) { c.Effort = effort.High }, tools: tools, drift: true},
		{name: "suspend on submit", mutate: func(c *Config[answer]) { c.SuspendOnSubmit = true }, tools: tools, drift: true},
		{name: "serving provider", mutate: func(c *Config[answer]) { c.Attribution.ProviderName = "openai" }, tools: tools, drift: true},
		{name: "output limit", mutate: func(c *Config[answer]) { c.MaxTokens = 999 }, tools: tools, drift: true},
		{name: "resource labels", mutate: func(c *Config[answer]) { c.ResourceLabels = map[string]string{"team": rand.Text()} }, tools: tools},
		{name: "turn budget", mutate: func(c *Config[answer]) { c.MaxTurns = 50 }, tools: tools},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := suspendConfig(t)
			tc.mutate(&cfg)
			resumed := newRecorder(t, []map[string]any{submit("s", "done")})
			_, err := asResumer(t, serve(t, resumed.handle, cfg)).Resume(t.Context(), env, map[string]string{"q": "because"}, tc.tools)
			if tc.drift {
				if !errors.Is(err, checkpoint.ErrConfigDrift) {
					t.Errorf("Resume: got = %v, want ErrConfigDrift", err)
				}
				if n := len(resumed.requests()); n != 0 {
					t.Errorf("requests after drift: got = %d, want = 0", n)
				}
				return
			}
			if err != nil {
				t.Errorf("Resume: got = %v, want success", err)
			}
		})
	}
}

// TestSuspendOnSubmitReviewRoundTrip: an accepted submission returns its
// Response with a ReasonSubmitted suspension. A sibling ask is answered
// not_asked, a second submission in the turn gets an error result, and the
// review goes back verbatim as the submit call's output.
func TestSuspendOnSubmitReviewRoundTrip(t *testing.T) {
	t.Parallel()
	cfg := suspendConfig(t)
	cfg.SuspendOnSubmit = true
	parked := newRecorder(t, []map[string]any{ask("q", "unsure?"), submit("s1", "draft"), submit("s2", "other")})
	got, err := serve(t, parked.handle, cfg).Execute(t.Context(), request{}, nil)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got = %v, want a suspension", err)
	}
	if got.Text != "draft" {
		t.Errorf("accepted response: got = %q, want = %q", got.Text, "draft")
	}
	if susp.Reason != checkpoint.ReasonSubmitted || susp.Question != "" || len(susp.PendingToolCalls) != 1 || susp.PendingToolCalls[0].ID != "s1" {
		t.Fatalf("suspension: got = %+v, want ReasonSubmitted pending on s1", susp.Envelope)
	}

	review := "revise: " + rand.Text()
	resumed := newRecorder(t, []map[string]any{submit("s3", "final")})
	got, err = asResumer(t, serve(t, resumed.handle, cfg)).ResumeWithResults(t.Context(), roundTrip(t, susp.Envelope), map[string]string{"s1": review}, nil)
	again, ok := checkpoint.AsSuspension(err)
	if !ok || got.Text != "final" || again.Turn != 1 {
		t.Fatalf("ResumeWithResults: got = %+v, %v, want the revised submission parked at turn 1", got, err)
	}
	items := inputItems(t, resumed.requests()[0])
	outputs := make(map[string]string)
	for _, item := range items {
		if item["type"] == "function_call_output" {
			outputs[item["call_id"].(string)] = item["output"].(string)
		}
	}
	if outputs["s1"] != review {
		t.Errorf("review output: got = %q, want = %q verbatim", outputs["s1"], review)
	}
	if !strings.Contains(outputs["q"], "not_asked") {
		t.Errorf("sibling ask output: got = %q, want not_asked", outputs["q"])
	}
	if !strings.Contains(outputs["s2"], "only one submission per turn is reviewed") {
		t.Errorf("second submission output: got = %q, want an error result", outputs["s2"])
	}
}

func TestSecondSuspendCallGetsAnError(t *testing.T) {
	t.Parallel()
	parked := newRecorder(t, []map[string]any{ask("q1", "first?"), ask("q2", "second?")})
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, nil)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || len(susp.PendingToolCalls) != 1 || susp.PendingToolCalls[0].ID != "q1" {
		t.Fatalf("Execute: got = %v, want a suspension pending on q1", err)
	}
	resumed := newRecorder(t, []map[string]any{submit("s", "done")})
	if _, err := asResumer(t, serve(t, resumed.handle, suspendConfig(t))).Resume(t.Context(), roundTrip(t, susp.Envelope), map[string]string{"q1": "yes"}, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	items := inputItems(t, resumed.requests()[0])
	last := items[len(items)-1]
	if last["call_id"] != "q2" || !strings.Contains(last["output"].(string), "only one question per turn is asked") {
		t.Errorf("second ask output: got = %v, want an error result", last)
	}
}

// TestSubmitWithoutSuspendOnSubmitEndsTheRun: an ask beside an accepted
// submission does not pause a run that ends on submission.
func TestSubmitWithoutSuspendOnSubmitEndsTheRun(t *testing.T) {
	t.Parallel()
	parked := newRecorder(t, []map[string]any{ask("q", "unsure?"), submit("s", "done")})
	got, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, nil)
	if err != nil || got.Text != "done" {
		t.Fatalf("Execute: got = %+v, %v, want the accepted result", got, err)
	}
}

func TestResumeRejectsUnpairableResults(t *testing.T) {
	t.Parallel()
	parked := newRecorder(t, []map[string]any{ask("q", "why?")})
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, nil)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got = %v, want a suspension", err)
	}
	env := roundTrip(t, susp.Envelope)
	foreign := env
	foreign.PendingToolCalls = []checkpoint.PendingToolCall{{ID: "not-in-turn", Name: askTool}}
	// A state whose input ends after the paused turn's outputs no longer ends
	// with the turn that paused.
	var state providerState
	if err := json.Unmarshal(env.ProviderState, &state); err != nil {
		t.Fatal(err)
	}
	state.Input = append(state.Input, json.RawMessage(`{"type":"function_call_output","call_id":"q","output":"stale"}`))
	moved := env
	if moved.ProviderState, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		resume  func(r interface{ ResultResumer[request, answer] }) error
		wantErr string
	}{
		{name: "missing result", wantErr: "no result for pending call", resume: func(r interface{ ResultResumer[request, answer] }) error {
			_, err := r.ResumeWithResults(t.Context(), env, map[string]string{}, nil)
			return err
		}},
		{name: "paused turn not last", wantErr: "does not end with a turn of function calls", resume: func(r interface{ ResultResumer[request, answer] }) error {
			_, err := r.ResumeWithResults(t.Context(), moved, map[string]string{"q": "x"}, nil)
			return err
		}},
		{name: "call not in the paused turn", wantErr: "neither a saved output nor an answer", resume: func(r interface{ ResultResumer[request, answer] }) error {
			_, err := r.ResumeWithResults(t.Context(), foreign, map[string]string{"not-in-turn": "x"}, nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resumed := newRecorder(t)
			err := tc.resume(asResumer(t, serve(t, resumed.handle, suspendConfig(t))))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("resume: got = %v, want containing %q", err, tc.wantErr)
			}
			if n := len(resumed.requests()); n != 0 {
				t.Errorf("requests: got = %d, want = 0", n)
			}
		})
	}
}

func TestSuspendToolCollisions(t *testing.T) {
	t.Parallel()
	cfg := suspendConfig(t)
	cfg.SuspendTool.Name = "submit_result"
	if _, err := New[request](responses.NewResponseService(), cfg); err == nil || !strings.Contains(err.Error(), "collides with the submit tool") {
		t.Errorf("New with a suspend tool named like submit: got = %v", err)
	}
	var lookups atomic.Int32
	shadow := lookupTool(&lookups)
	shadow.Def.Name = askTool
	parked := newRecorder(t)
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(t.Context(), request{}, map[string]toolcall.Tool[answer]{askTool: shadow})
	if err == nil || !strings.Contains(err.Error(), "collides with a caller-registered tool") {
		t.Errorf("Execute with a caller tool named like suspend: got = %v", err)
	}
}

// TestSuspendedTurnIsNotFailed: the trace records the suspension, not a
// failed turn.
func TestSuspendedTurnIsNotFailed(t *testing.T) {
	t.Parallel()
	parked := newRecorder(t, []map[string]any{ask("q", "why?")})
	tracer := new(recordingTracer)
	_, err := serve(t, parked.handle, suspendConfig(t)).Execute(agenttrace.WithTracer[answer](t.Context(), tracer), request{}, nil)
	if _, ok := checkpoint.AsSuspension(err); !ok {
		t.Fatalf("Execute: got = %v, want a suspension", err)
	}
	if tracer.trace == nil || len(tracer.trace.Turns) != 1 {
		t.Fatal("missing turn trace")
	}
	if turn := tracer.trace.Turns[0]; turn.Failed || len(turn.Errors) != 0 {
		t.Errorf("suspended turn: failed = %v, errors = %v, want neither", turn.Failed, turn.Errors)
	}
	if !tracer.trace.Suspended {
		t.Error("trace: got not suspended, want suspended")
	}
}
