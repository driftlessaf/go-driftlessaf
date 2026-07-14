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

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/cloudevents/sdk-go/v2/protocol"

	"chainguard.dev/driftlessaf/reconcilers/statemachine"
)

// stubCEClient records sent events. Request/StartReceiver are unused by the
// emitter; they panic to catch accidental use.
type stubCEClient struct {
	mu     sync.Mutex
	sent   []event.Event
	result protocol.Result
}

var _ cloudevents.Client = (*stubCEClient)(nil)

func (s *stubCEClient) Send(_ context.Context, e event.Event) protocol.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, e)
	return s.result
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

func TestSaveEmitsTransitionEvent(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"active"}`)
	r := newReconcilerForFixture(t, f)
	stub := &stubCEClient{}
	r.transitionEmitter = statemachine.NewEmitter(testActor, stub)
	mgr := r.NewStateManager(f.issue)
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	mgr.now = fixedClock(now)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusComplete)
	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRMerge)
	if _, err := mgr.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sent := stub.events()
	if len(sent) != 1 {
		t.Fatalf("sent events: got = %d, want = 1", len(sent))
	}
	var got StateTransitionEvent
	if err := json.Unmarshal(sent[0].Data(), &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.FromStatus != StatusActive || got.ToStatus != StatusComplete {
		t.Errorf("transition: got = %s->%s, want = active->complete", got.FromStatus, got.ToStatus)
	}
	if got.TransitionAt != now {
		t.Errorf("transition_at: got = %v, want = %v", got.TransitionAt, now)
	}
	// Lock the manager.go field mapping: these come from the fixture's
	// issue and loaded state, not from the emitter-level shape test.
	if got.Bot != testActor {
		t.Errorf("bot: got = %q, want = %q", got.Bot, testActor)
	}
	if got.Provider != stateTransitionProvider {
		t.Errorf("provider: got = %q, want = %q", got.Provider, stateTransitionProvider)
	}
	if got.IssueID != f.issue.ID {
		t.Errorf("issue_id: got = %q, want = %q", got.IssueID, f.issue.ID)
	}
	if got.IssueURL != f.issue.URL {
		t.Errorf("issue_url: got = %q, want = %q", got.IssueURL, f.issue.URL)
	}
	if want := "https://github.com/o/r/pull/1"; got.PRURL != want {
		t.Errorf("pr_url: got = %q, want = %q", got.PRURL, want)
	}

	// A second Save with no further changes must not emit again.
	if _, err := mgr.Save(ctx, s); err != nil {
		t.Fatalf("no-op Save: %v", err)
	}
	if got := len(stub.events()); got != 1 {
		t.Errorf("events after no-op save: got = %d, want = 1", got)
	}
}

func TestSaveFailureEmitsNothing(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"active"}`)
	r := newReconcilerForFixture(t, f)
	stub := &stubCEClient{}
	r.transitionEmitter = statemachine.NewEmitter(testActor, stub)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusComplete)
	f.failSaves.Store(true)

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRMerge)
	if _, err := mgr.Save(ctx, s); err == nil {
		t.Fatalf("Save: got nil error, want save failure")
	}
	if got := len(stub.events()); got != 0 {
		t.Errorf("events after failed save: got = %d, want = 0", got)
	}
}
