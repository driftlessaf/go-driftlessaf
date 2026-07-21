/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/jsonlstore"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/suspend/memquestions"
	"chainguard.dev/driftlessaf/workqueue"
)

// fakeAgent is a scripted metaagent.Agent + metaagent.Resumer stand-in that
// avoids any network. Its first Execute suspends (as if the model called the
// ask-a-friend tool); Resume records the answers it was handed and returns a
// completed result. It lets the demo's tri-state lifecycle be exercised
// deterministically with the real Coordinator and real checkpoint/question
// stores.
type fakeAgent struct {
	// suspendCallIDs are the pending tool-call IDs Execute suspends with. The
	// lifecycle test hands it TWO calls to the SAME tool with distinct IDs: an
	// answers map keyed by tool name instead of the persisted call ID would
	// collapse them into a single colliding entry and fail the per-ID pairing
	// assertions, so the ID-vs-name distinction is actually pinned.
	suspendCallIDs []string
	question       string
	// resumeAnswers captures the answers map handed to Resume, so a test can
	// assert the answer was paired to the persisted pending tool-call ID rather
	// than re-derived from the tool name.
	resumeAnswers map[string]string
	resumed       bool
	// resumeSuspendsWith, when non-empty, makes the next Resume suspend again
	// with this question (a multi-question conversation) instead of completing;
	// it is cleared so the following Resume completes.
	resumeSuspendsWith string
	// resumeErr, when set, makes the next Resume fail with it (a transient
	// provider error); it is cleared so the following Resume completes.
	resumeErr error
	// executeResult, when set, makes Execute complete with it instead of
	// suspending — the ordinary autofix outcome when the model never asks.
	executeResult *PRFixResult
}

var _ metaagent.Resumer[*PRContext, *PRFixResult, PRTools] = (*fakeAgent)(nil)

func (f *fakeAgent) Execute(_ context.Context, _ *PRContext, _ PRTools) (*PRFixResult, error) {
	if f.executeResult != nil {
		return f.executeResult, nil
	}
	pending := make([]checkpoint.PendingToolCall, 0, len(f.suspendCallIDs))
	for _, id := range f.suspendCallIDs {
		pending = append(pending, checkpoint.PendingToolCall{ID: id, Name: askAFriendToolName})
	}
	return nil, &checkpoint.Suspension{
		Envelope: checkpoint.Envelope{
			Version:          checkpoint.EnvelopeVersion,
			Provider:         checkpoint.ProviderAnthropic,
			Model:            "claude-test",
			RunID:            "run-1",
			Turn:             1,
			RemainingTurns:   5,
			Reason:           checkpoint.ReasonAwaitingAnswer,
			PendingToolCalls: pending,
			// Coordinator.Suspend validates the envelope before persisting it: a
			// real executor always captures provider state AND stamps a config
			// digest (park-time Validate fails closed on a missing digest), so
			// the fake must too.
			ProviderState: json.RawMessage(`{"messages":[]}`),
			ConfigDigest:  "test-digest",
		},
		Question: f.question,
	}
}

func (f *fakeAgent) Resume(_ context.Context, env checkpoint.Envelope, answers map[string]string, _ PRTools) (*PRFixResult, error) {
	f.resumeAnswers = answers
	f.resumed = true
	if err := f.resumeErr; err != nil {
		f.resumeErr = nil
		return nil, err
	}
	if q := f.resumeSuspendsWith; q != "" {
		f.resumeSuspendsWith = ""
		env.Turn++
		env.PendingToolCalls = []checkpoint.PendingToolCall{{ID: "toolu_ask_again", Name: askAFriendToolName}}
		return nil, &checkpoint.Suspension{Envelope: env, Question: q}
	}
	return &PRFixResult{Success: true, FixesApplied: []string{"applied after human answer"}, Reasoning: "resumed"}, nil
}

// newTestReconciler wires a real Coordinator over a temp-file jsonlstore and an
// in-memory question store, handing every wake the same fakeAgent so the test
// can inspect what Resume received.
func newTestReconciler(t *testing.T, agent *fakeAgent) (*askAFriendReconciler, *memquestions.Store) {
	t.Helper()
	store, err := jsonlstore.New(filepath.Join(t.TempDir(), "checkpoints.jsonl"))
	if err != nil {
		t.Fatalf("jsonlstore.New: %v", err)
	}
	questions := memquestions.New()
	coord, err := suspend.New(store, questions, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("suspend.New: %v", err)
	}
	r := &askAFriendReconciler{
		coord: coord,
		newAgent: func(context.Context) (metaagent.Agent[*PRContext, *PRFixResult, PRTools], error) {
			return agent, nil
		},
	}
	return r, questions
}

// TestAskAFriendLifecycle drives the full suspend -> rearm -> resume path through
// the real Coordinator and stores: a fresh run begins exactly one fix attempt,
// suspends, and parks; a poll wake before an answer rearms without re-executing
// or beginning another attempt; and once answered the run resumes — without
// consulting the attempt gate — with the answer paired to each persisted
// pending tool-call ID. The fake suspends with TWO pending calls to the same
// tool under distinct IDs, so pairing by tool name (which would collapse them
// to one colliding entry) cannot pass.
func TestAskAFriendLifecycle(t *testing.T) {
	const key = "acme/widgets#7"
	const answer = "gcr.io/distroless/base"
	callIDs := []string{"toolu_ask_123", "toolu_ask_456"}
	agent := &fakeAgent{suspendCallIDs: callIDs, question: "Which base image should I target?"}
	r, questions := newTestReconciler(t, agent)
	ctx := t.Context()
	req := &PRContext{Owner: "acme", Repo: "widgets", PRNumber: 7}

	// beginFresh stands in for the reconciler's fix-attempt accounting; the
	// lifecycle must invoke it for the one fresh execution and NEVER for a
	// rearm poll or a resume, or parked runs would burn MAX_FIX_ATTEMPTS while
	// waiting on a human.
	freshBegins := 0
	beginFresh := func(context.Context) error {
		freshBegins++
		return nil
	}

	// 1. Fresh reconcile: the model asks a human, so run parks the checkpoint and
	// returns a requeue (the pause signal), not a result.
	result, err := r.run(ctx, key, req, PRTools{}, beginFresh)
	if result != nil {
		t.Fatalf("fresh suspend: want nil result, got %+v", result)
	}
	if _, ok := workqueue.GetRequeueDelay(err); !ok {
		t.Fatalf("fresh suspend: want a requeue error, got %v", err)
	}
	if freshBegins != 1 {
		t.Fatalf("fresh suspend: beginFresh called %d times, want 1", freshBegins)
	}

	// 2. Poll wake before the human answers: rearm, still a requeue, still no
	// result — and a rearm must NOT re-execute or resume the agent, nor begin
	// another fix attempt.
	result, err = r.run(ctx, key, req, PRTools{}, beginFresh)
	if result != nil {
		t.Fatalf("rearm: want nil result, got %+v", result)
	}
	if _, ok := workqueue.GetRequeueDelay(err); !ok {
		t.Fatalf("rearm: want a requeue error, got %v", err)
	}
	if agent.resumed {
		t.Fatal("rearm must not resume the agent")
	}
	if freshBegins != 1 {
		t.Fatalf("rearm: beginFresh called %d times, want still 1 (a poll must not burn a fix attempt)", freshBegins)
	}

	// 3. A human answers via the demo transport.
	if _, ok := questions.Provide(ctx, key, answer); !ok {
		t.Fatal("Provide: no pending question for key")
	}

	// 4. Wake after the answer: resume completes and returns a result, without
	// beginning a new fix attempt (the resume continues the fresh run's).
	result, err = r.run(ctx, key, req, PRTools{}, beginFresh)
	if err != nil {
		t.Fatalf("resume: unexpected error: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("resume: want a successful result, got %+v", result)
	}
	if !agent.resumed {
		t.Fatal("resume: agent.Resume was not called")
	}
	if freshBegins != 1 {
		t.Fatalf("resume: beginFresh called %d times, want still 1 (a resume continues the fresh run's attempt)", freshBegins)
	}

	// 5. Each answer was paired to a PERSISTED pending tool-call ID, never
	// re-derived from the tool name: both same-tool calls must appear under
	// their own distinct IDs (a name-keyed map would hold a single entry).
	if got := len(agent.resumeAnswers); got != len(callIDs) {
		t.Fatalf("resume: got %d answer entries %v, want one per persisted call ID %v", got, agent.resumeAnswers, callIDs)
	}
	for _, id := range callIDs {
		if got := agent.resumeAnswers[id]; got != answer {
			t.Fatalf("resume: answer for %q = %q, want the human answer keyed by the persisted call ID", id, got)
		}
	}
}

// TestAskAFriendFreshRunCompletes pins the ordinary outcome once ask-a-friend is
// enabled: a fresh run whose model never asks a human returns its result
// straight through — one fix attempt begun, nothing parked, no question
// posted, and no requeue.
func TestAskAFriendFreshRunCompletes(t *testing.T) {
	const key = "acme/widgets#13"
	agent := &fakeAgent{
		executeResult: &PRFixResult{Success: true, FixesApplied: []string{"title fixed"}, Reasoning: "no human needed"},
	}
	r, questions := newTestReconciler(t, agent)
	ctx := t.Context()
	freshBegins := 0
	beginFresh := func(context.Context) error {
		freshBegins++
		return nil
	}

	result, err := r.run(ctx, key, &PRContext{Owner: "acme", Repo: "widgets", PRNumber: 13}, PRTools{}, beginFresh)
	if err != nil {
		t.Fatalf("fresh completion: unexpected error: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("fresh completion: want the successful result, got %+v", result)
	}
	if freshBegins != 1 {
		t.Errorf("beginFresh calls: got = %d, want = 1", freshBegins)
	}
	if _, ok, err := questions.Pending(ctx, key); err != nil || ok {
		t.Errorf("Pending after completed fresh run: ok=%v err=%v, want nothing posted", ok, err)
	}
	if agent.resumed {
		t.Error("a completed fresh run must not call Resume")
	}
}

// TestAskAFriendResumeCanSuspendAgain pins the multi-question conversation: a
// resumed run that calls the ask-a-friend tool again must RE-PARK (a requeue and
// a fresh pending question), never fall through to the caller as a terminal
// error — and the second pause must still burn no additional fix attempt.
func TestAskAFriendResumeCanSuspendAgain(t *testing.T) {
	const key = "acme/widgets#9"
	agent := &fakeAgent{
		suspendCallIDs:     []string{"toolu_ask_1"},
		question:           "first question?",
		resumeSuspendsWith: "second question?",
	}
	r, questions := newTestReconciler(t, agent)
	ctx := t.Context()
	req := &PRContext{Owner: "acme", Repo: "widgets", PRNumber: 9}
	freshBegins := 0
	beginFresh := func(context.Context) error {
		freshBegins++
		return nil
	}

	// Fresh run parks on the first question; the human answers it.
	if _, err := r.run(ctx, key, req, PRTools{}, beginFresh); err == nil {
		t.Fatal("fresh suspend: want a requeue error, got nil")
	}
	if _, ok := questions.Provide(ctx, key, "answer one"); !ok {
		t.Fatal("Provide (first question): no pending question for key")
	}

	// The wake resumes, and the resumed run suspends AGAIN: the outcome must
	// be a re-park (requeue + the second question pending), not a result and
	// not a terminal error.
	result, err := r.run(ctx, key, req, PRTools{}, beginFresh)
	if result != nil {
		t.Fatalf("re-suspension: want nil result, got %+v", result)
	}
	if _, ok := workqueue.GetRequeueDelay(err); !ok {
		t.Fatalf("re-suspension: want a requeue error, got %v", err)
	}
	q, ok, err := questions.Pending(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Pending after re-suspension: ok=%v err=%v", ok, err)
	}
	if q.Prompt != "second question?" {
		t.Fatalf("Pending prompt: got = %q, want the second question", q.Prompt)
	}

	// Answering the second question completes the run.
	if _, ok := questions.Provide(ctx, key, "answer two"); !ok {
		t.Fatal("Provide (second question): no pending question for key")
	}
	result, err = r.run(ctx, key, req, PRTools{}, beginFresh)
	if err != nil {
		t.Fatalf("final resume: unexpected error: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("final resume: want a successful result, got %+v", result)
	}
	if got := agent.resumeAnswers["toolu_ask_again"]; got != "answer two" {
		t.Fatalf("final resume: answer for the re-suspended call = %q, want %q", got, "answer two")
	}
	if freshBegins != 1 {
		t.Fatalf("beginFresh called %d times across the multi-question lifecycle, want 1", freshBegins)
	}
}

// TestAskAFriendResumeFailureSurfacesForRetry pins the claimed-run failure
// contract: a non-suspension Resume error happens AFTER the wake CAS-deleted
// the envelope and consumed the answer, so run must return an error marked
// errResumeFailed (which reconcilePR passes to the workqueue for retry) —
// never a requeue and never a result — and the retry wake must find no
// checkpoint and re-run from scratch.
func TestAskAFriendResumeFailureSurfacesForRetry(t *testing.T) {
	const key = "acme/widgets#11"
	transient := errors.New("vertex: 529 overloaded")
	agent := &fakeAgent{
		suspendCallIDs: []string{"toolu_ask_1"},
		question:       "which one?",
		resumeErr:      transient,
	}
	r, questions := newTestReconciler(t, agent)
	ctx := t.Context()
	req := &PRContext{Owner: "acme", Repo: "widgets", PRNumber: 11}
	freshBegins := 0
	beginFresh := func(context.Context) error {
		freshBegins++
		return nil
	}

	// Park, answer, then wake into the failing Resume.
	if _, err := r.run(ctx, key, req, PRTools{}, beginFresh); err == nil {
		t.Fatal("fresh suspend: want a requeue error, got nil")
	}
	if _, ok := questions.Provide(ctx, key, "the answer"); !ok {
		t.Fatal("Provide: no pending question for key")
	}
	result, err := r.run(ctx, key, req, PRTools{}, beginFresh)
	if result != nil {
		t.Fatalf("failed resume: want nil result, got %+v", result)
	}
	if !errors.Is(err, errResumeFailed) || !errors.Is(err, transient) {
		t.Fatalf("failed resume: err = %v, want errResumeFailed wrapping the transient error", err)
	}
	if _, ok := workqueue.GetRequeueDelay(err); ok {
		t.Fatalf("failed resume: must not be a requeue (the claim is gone; only a workqueue retry re-runs): %v", err)
	}

	// The workqueue retry finds no checkpoint: WakeFresh re-runs from scratch,
	// beginning a second fix attempt (the fake suspends again on Execute).
	if _, err := r.run(ctx, key, req, PRTools{}, beginFresh); err == nil {
		t.Fatal("retry wake: want a requeue error from the fresh re-park, got nil")
	}
	if freshBegins != 2 {
		t.Fatalf("beginFresh calls: got = %d, want = 2 (initial run + post-failure re-run)", freshBegins)
	}
}

// TestAskAFriendRequiresClaudeModel pins newAskAFriendReconciler's fail-fast model
// guard: suspend/resume is only wired for the Claude backend today, so a
// non-claude AGENT_MODEL must fail at startup rather than on the first
// reconcile.
func TestAskAFriendRequiresClaudeModel(t *testing.T) {
	cfg := &config{
		Model:                  "gemini-2.5-flash",
		AskAFriendWakeInterval: time.Second,
		AskAFriendSnapshotPath: filepath.Join(t.TempDir(), "checkpoints.jsonl"),
	}
	if _, err := newAskAFriendReconciler(t.Context(), cfg, nil); err == nil {
		t.Fatal("newAskAFriendReconciler: want an error for a non-claude model, got nil")
	}
}
