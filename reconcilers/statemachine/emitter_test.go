/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statemachine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/cloudevents/sdk-go/v2/protocol"
)

const testBot = "test-bot"

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

func TestEmitterSendsEvent(t *testing.T) {
	stub := &stubCEClient{}
	e := NewEmitter(testBot, stub)

	want := StateTransitionEvent{
		Bot:          testBot,
		Provider:     "linear",
		IssueID:      "iss-123",
		IssueURL:     "https://linear.app/x/issue/ABC-1",
		PRURL:        "https://github.com/o/r/pull/1",
		FromStatus:   StatusActive,
		ToStatus:     StatusFailed,
		FailureMode:  FailureModeMaxTurns,
		Actor:        testBot,
		Trigger:      TriggerPRMerge,
		TransitionAt: time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
	}
	e.Emit(t.Context(), want)

	sent := stub.events()
	if len(sent) != 1 {
		t.Fatalf("sent events: got = %d, want = 1", len(sent))
	}
	if got := sent[0].Type(); got != StateTransitionEventType {
		t.Errorf("event type: got = %q, want = %q", got, StateTransitionEventType)
	}
	if got := sent[0].Source(); got != testBot {
		t.Errorf("event source: got = %q, want = %q", got, testBot)
	}
	var got StateTransitionEvent
	if err := json.Unmarshal(sent[0].Data(), &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got != want {
		t.Errorf("payload: got = %+v, want = %+v", got, want)
	}
}

func TestEmitterNilIsNoOp(t *testing.T) {
	// A nil emitter (emission not wired) must be a safe no-op.
	var nilEmitter *Emitter
	nilEmitter.Emit(t.Context(), StateTransitionEvent{Bot: testBot})
}

func TestEmitterSwallowsSendErrors(t *testing.T) {
	stub := &stubCEClient{result: errors.New("broker unreachable")}
	e := NewEmitter(testBot, stub)
	// Must not panic or propagate; the caller's save has already succeeded.
	e.Emit(t.Context(), StateTransitionEvent{Bot: testBot, TransitionAt: time.Now().UTC()})
	if got := len(stub.events()); got != 1 {
		t.Errorf("send attempts: got = %d, want = 1", got)
	}
}

func TestNewEmitter(t *testing.T) {
	// A nil client (e.g. agenttrace.NewBrokerClient with an empty URI)
	// disables emission entirely.
	if e := NewEmitter(testBot, nil); e != nil {
		t.Errorf("nil-client emitter: got = %+v, want = nil", e)
	}

	stub := &stubCEClient{}
	e := NewEmitter(testBot, stub)
	if e == nil {
		t.Fatal("emitter: got = nil, want non-nil")
	}
	if e.Source() != testBot {
		t.Errorf("source: got = %q, want = %q", e.Source(), testBot)
	}
	if e.client != cloudevents.Client(stub) {
		t.Errorf("client: got = %v, want the supplied stub", e.client)
	}
}

func TestContextRoundTrips(t *testing.T) {
	ctx := WithActor(t.Context(), "some-bot")
	ctx = WithTrigger(ctx, TriggerInitialRun)

	if actor, ok := ActorFromContext(ctx); !ok || actor != "some-bot" {
		t.Errorf("actor: got = (%q, %v), want = (\"some-bot\", true)", actor, ok)
	}
	if trigger, ok := TriggerFromContext(ctx); !ok || trigger != TriggerInitialRun {
		t.Errorf("trigger: got = (%q, %v), want = (%q, true)", trigger, ok, TriggerInitialRun)
	}

	if _, ok := ActorFromContext(t.Context()); ok {
		t.Error("actor on empty context: got ok = true, want false")
	}
	if _, ok := TriggerFromContext(t.Context()); ok {
		t.Error("trigger on empty context: got ok = true, want false")
	}
}
