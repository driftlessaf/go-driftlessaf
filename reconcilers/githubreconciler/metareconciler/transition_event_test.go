/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/statemachine"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/cloudevents/sdk-go/v2/protocol"
	"github.com/google/go-github/v88/github"
)

// stubCEClient records sent events. Request/StartReceiver are unused by the
// emitter; they panic to catch accidental use.
type stubCEClient struct {
	mu   sync.Mutex
	sent []event.Event
}

var _ cloudevents.Client = (*stubCEClient)(nil)

func (s *stubCEClient) Send(_ context.Context, e event.Event) protocol.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, e)
	return nil
}

func (s *stubCEClient) Request(context.Context, event.Event) (*event.Event, protocol.Result) {
	panic("unused")
}

func (s *stubCEClient) StartReceiver(context.Context, any) error {
	panic("unused")
}

func (s *stubCEClient) events() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.Event(nil), s.sent...)
}

func newTestReconciler(t *testing.T, opts ...Option[*testRequest, *testResult, testCallbacks]) *Reconciler[*testRequest, *testResult, testCallbacks] {
	t.Helper()
	return New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		&fakeAgent{},
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		opts...,
	)
}

func TestWithStateTransitionEmission(t *testing.T) {
	// Without the option (or with a nil client, e.g. agenttrace.NewBrokerClient
	// on an empty URI) the emitter is nil and emission is disabled.
	if rec := newTestReconciler(t); rec.transitionEmitter != nil {
		t.Errorf("transitionEmitter without option: got = %+v, want = nil", rec.transitionEmitter)
	}
	if rec := newTestReconciler(t, WithStateTransitionEmission[*testRequest, *testResult, testCallbacks](nil)); rec.transitionEmitter != nil {
		t.Errorf("transitionEmitter with nil client: got = %+v, want = nil", rec.transitionEmitter)
	}

	rec := newTestReconciler(t, WithStateTransitionEmission[*testRequest, *testResult, testCallbacks](&stubCEClient{}))
	if rec.transitionEmitter == nil {
		t.Fatal("transitionEmitter: got = nil, want an emitter")
	}
	if got := rec.transitionEmitter.Source(); got != "test-identity" {
		t.Errorf("emitter source: got = %q, want = %q", got, "test-identity")
	}
}

func TestEmitTransition(t *testing.T) {
	stub := &stubCEClient{}
	rec := newTestReconciler(t, WithStateTransitionEmission[*testRequest, *testResult, testCallbacks](stub))

	issue := &github.Issue{
		HTMLURL: github.Ptr("https://github.com/o/r/issues/123"),
	}
	rec.emitTransition(t.Context(), issue, "https://github.com/o/r/pull/7",
		statemachine.StatusFailed, statemachine.FailureModeMaxTurns, statemachine.TriggerMaxTurns)

	sent := stub.events()
	if len(sent) != 1 {
		t.Fatalf("sent events: got = %d, want = 1", len(sent))
	}
	if got := sent[0].Type(); got != StateTransitionEventType {
		t.Errorf("event type: got = %q, want = %q", got, StateTransitionEventType)
	}
	var got StateTransitionEvent
	if err := json.Unmarshal(sent[0].Data(), &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.Bot != "test-identity" {
		t.Errorf("bot: got = %q, want = %q", got.Bot, "test-identity")
	}
	if got.Provider != "github" {
		t.Errorf("provider: got = %q, want = %q", got.Provider, "github")
	}
	// The GitHub flavor keys work by issue URL; IssueID and IssueURL coincide.
	if got.IssueID != "https://github.com/o/r/issues/123" {
		t.Errorf("issue_id: got = %q, want the issue URL", got.IssueID)
	}
	if got.IssueURL != "https://github.com/o/r/issues/123" {
		t.Errorf("issue_url: got = %q, want the issue URL", got.IssueURL)
	}
	if got.PRURL != "https://github.com/o/r/pull/7" {
		t.Errorf("pr_url: got = %q, want the PR URL", got.PRURL)
	}
	// No persisted prior state on the GitHub side; from_status stays empty.
	if got.FromStatus != "" {
		t.Errorf("from_status: got = %q, want empty", got.FromStatus)
	}
	if got.ToStatus != statemachine.StatusFailed {
		t.Errorf("to_status: got = %q, want = %q", got.ToStatus, statemachine.StatusFailed)
	}
	if got.FailureMode != statemachine.FailureModeMaxTurns {
		t.Errorf("failure_mode: got = %q, want = %q", got.FailureMode, statemachine.FailureModeMaxTurns)
	}
	if got.Actor != "test-identity" {
		t.Errorf("actor: got = %q, want = %q", got.Actor, "test-identity")
	}
	if got.Trigger != statemachine.TriggerMaxTurns {
		t.Errorf("trigger: got = %q, want = %q", got.Trigger, statemachine.TriggerMaxTurns)
	}
	if got.TransitionAt.IsZero() || time.Since(got.TransitionAt) > time.Minute {
		t.Errorf("transition_at: got = %v, want a recent timestamp", got.TransitionAt)
	}

	// A reconciler without emission wired must no-op, not panic.
	recOff := newTestReconciler(t)
	recOff.emitTransition(t.Context(), issue, "",
		statemachine.StatusActive, "", statemachine.TriggerInitialRun)
}

func TestUpsertTrigger(t *testing.T) {
	tests := []struct {
		name        string
		wasNoPR     bool
		usePRBranch bool
		needsRebase bool
		want        string
	}{{
		name:    "no PR yet is an initial run",
		wasNoPR: true,
		want:    statemachine.TriggerInitialRun,
	}, {
		name:        "findings iteration on the PR branch",
		usePRBranch: true,
		want:        statemachine.TriggerCIFailureIteration,
	}, {
		name:        "regeneration after merge conflict",
		needsRebase: true,
		want:        statemachine.TriggerMergeConflict,
	}, {
		name: "unexpected state combination carries no trigger",
		want: "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upsertTrigger(tt.wasNoPR, tt.usePRBranch, tt.needsRebase); got != tt.want {
				t.Errorf("upsertTrigger(%v, %v, %v): got = %q, want = %q", tt.wasNoPR, tt.usePRBranch, tt.needsRebase, got, tt.want)
			}
		})
	}
}

func TestClosedIssueTransition(t *testing.T) {
	tests := []struct {
		name        string
		hadPR       bool
		stateReason string
		wantStatus  statemachine.Status
		wantMode    statemachine.FailureMode
	}{{
		name:        "open PR closed under a completed issue is abandoned work",
		hadPR:       true,
		stateReason: "completed",
		wantStatus:  statemachine.StatusFailed,
		wantMode:    statemachine.FailureModePRClosed,
	}, {
		name:        "open PR closed under a not-planned issue is abandoned work",
		hadPR:       true,
		stateReason: "not_planned",
		wantStatus:  statemachine.StatusFailed,
		wantMode:    statemachine.FailureModePRClosed,
	}, {
		name:        "no PR and completed is the merge auto-close observation",
		stateReason: "completed",
		wantStatus:  statemachine.StatusComplete,
	}, {
		name:        "no PR and not planned is discarded work",
		stateReason: "not_planned",
		wantStatus:  statemachine.StatusFailed,
	}, {
		name:        "no PR and duplicate is discarded work",
		stateReason: "duplicate",
		wantStatus:  statemachine.StatusFailed,
	}, {
		name:       "no PR and empty state reason is discarded work",
		wantStatus: statemachine.StatusFailed,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotMode := closedIssueTransition(tt.hadPR, tt.stateReason)
			if gotStatus != tt.wantStatus {
				t.Errorf("closedIssueTransition(%v, %q) status: got = %q, want = %q", tt.hadPR, tt.stateReason, gotStatus, tt.wantStatus)
			}
			if gotMode != tt.wantMode {
				t.Errorf("closedIssueTransition(%v, %q) mode: got = %q, want = %q", tt.hadPR, tt.stateReason, gotMode, tt.wantMode)
			}
		})
	}
}
