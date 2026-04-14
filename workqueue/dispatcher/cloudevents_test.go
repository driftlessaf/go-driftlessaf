/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/workqueue"
	cloudevents "github.com/cloudevents/sdk-go/v2"
)

func TestWithErrorBrokerURL_EmptyIsNop(t *testing.T) {
	// Empty broker URL should leave the nop emitter in place.
	opt := WithErrorBrokerURL(context.Background(), "", "test-wq")
	cfg := config{errors: nopErrorEmitter{}}
	opt(&cfg)

	// Should still be the nop emitter (no panic on emit).
	cfg.errors.emit(context.Background(), ErrorContext{
		Key: "some-key", Err: errors.New("fail"), Attempts: 1, Action: ErrorRequeued,
	})
	cfg.errors.drain()
}

func TestCloudEventErrorEmitter_EmitsEvent(t *testing.T) {
	var mu sync.Mutex
	var received *cloudevents.Event

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event, err := cloudevents.NewEventFromHTTPRequest(r)
		if err != nil {
			t.Errorf("failed to parse event: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = event
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ceClient, err := cloudevents.NewClientHTTP(cloudevents.WithTarget(srv.URL))
	if err != nil {
		t.Fatalf("create ce client: %v", err)
	}

	wqName := fmt.Sprintf("test-wq-%d", rand.Int64())
	e := &cloudEventErrorEmitter{
		client:        ceClient,
		workqueueName: wqName,
	}
	e.eg.SetLimit(ceMaxInflight)

	key := fmt.Sprintf("test-key-%d", rand.Int64())
	e.emit(context.Background(), ErrorContext{
		Key:      key,
		Err:      errors.New("something broke"),
		Attempts: 3,
		Action:   ErrorDeadLettered,
	})
	e.drain()

	mu.Lock()
	defer mu.Unlock()

	if received == nil {
		t.Fatal("no event received")
	}
	if received.Type() != ErrorEventType {
		t.Errorf("type: got = %q, wanted = %q", received.Type(), ErrorEventType)
	}
	if received.Source() != wqName {
		t.Errorf("source: got = %q, wanted = %q", received.Source(), wqName)
	}
	if received.Subject() != key {
		t.Errorf("subject: got = %q, wanted = %q", received.Subject(), key)
	}
	if action, _ := received.Extensions()["action"].(string); action != "dead-lettered" {
		t.Errorf("extension action: got = %q, wanted = %q", action, "dead-lettered")
	}

	var payload DispatcherErrorEvent
	if err := json.Unmarshal(received.Data(), &payload); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if payload.Key != key {
		t.Errorf("payload.Key: got = %q, wanted = %q", payload.Key, key)
	}
	if payload.Error != "something broke" {
		t.Errorf("payload.Error: got = %q, wanted = %q", payload.Error, "something broke")
	}
	if payload.Attempts != 3 {
		t.Errorf("payload.Attempts: got = %d, wanted = 3", payload.Attempts)
	}
	if payload.Action != "dead-lettered" {
		t.Errorf("payload.Action: got = %q, wanted = %q", payload.Action, "dead-lettered")
	}
}

func TestCloudEventErrorEmitter_IntegrationWithDispatcher(t *testing.T) {
	var mu sync.Mutex
	var received *cloudevents.Event

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event, err := cloudevents.NewEventFromHTTPRequest(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = event
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ceClient, err := cloudevents.NewClientHTTP(cloudevents.WithTarget(srv.URL))
	if err != nil {
		t.Fatalf("create ce client: %v", err)
	}

	e := &cloudEventErrorEmitter{
		client:        ceClient,
		workqueueName: "integration-test",
	}
	e.eg.SetLimit(ceMaxInflight)

	// Wire the emitter into the dispatcher config via the option mechanism.
	opt := func(c *config) { c.errors = e }

	key := fmt.Sprintf("integration-%d", rand.Int64())
	next := &mockKey{name: key, attempts: 10}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("boom")
	}, 10, opt)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// future() already calls cfg.errors.drain() internally.

	mu.Lock()
	defer mu.Unlock()

	if received == nil {
		t.Fatal("no event received from dispatcher")
	}

	var payload DispatcherErrorEvent
	if err := json.Unmarshal(received.Data(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Key != key {
		t.Errorf("payload.Key: got = %q, wanted = %q", payload.Key, key)
	}
	if payload.Action != "dead-lettered" {
		t.Errorf("payload.Action: got = %q, wanted = %q", payload.Action, "dead-lettered")
	}
}
