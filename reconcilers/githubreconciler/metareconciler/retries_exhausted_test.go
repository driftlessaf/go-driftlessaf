/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"chainguard.dev/driftlessaf/workqueue"
)

// withRetriesExhausted configures the retries-exhausted note the tests look for.
func withRetriesExhausted() Option[*testRequest, *publishResult, struct{}] {
	return WithRetriesExhaustedNote[*testRequest, *publishResult, struct{}](func(attempts int, _ string) string {
		return fmt.Sprintf("gave up after %d attempts", attempts)
	})
}

// agentFails is an agent whose run fails, so the reconcile returns an error.
func agentFails(context.Context) error { return errors.New("model unavailable") }

// TestReconcileIssueRetriesExhaustedNote proves the note goes on the issue only
// when the failure ends the workqueue's attempts, so a filer whose run was
// dead-lettered or dropped learns of it, and a run that will be retried posts
// nothing that would then answer the issue before the retry.
func TestReconcileIssueRetriesExhaustedNote(t *testing.T) {
	tests := []struct {
		name     string
		attempt  *workqueue.Attempt
		agent    func(context.Context) error
		opts     []Option[*testRequest, *publishResult, struct{}]
		wantNote string
	}{{
		name:     "the last attempt fails",
		attempt:  &workqueue.Attempt{Number: 5, Max: 5},
		agent:    agentFails,
		opts:     []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
		wantNote: "gave up after 5 attempts",
	}, {
		name:    "an attempt with retries left fails",
		attempt: &workqueue.Attempt{Number: 4, Max: 5},
		agent:   agentFails,
		opts:    []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
	}, {
		name:    "a non-retriable failure with retries left",
		attempt: &workqueue.Attempt{Number: 1, Max: 5},
		agent: func(context.Context) error {
			return workqueue.NonRetriableError(errors.New("bad request"), "bad request")
		},
		opts:     []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
		wantNote: "gave up after 1 attempts",
	}, {
		// An executor whose model stays overloaded past its own retries asks
		// for a requeue. The requeue restarts the key's attempt count, and a
		// note would answer the issue before the requeued reconcile runs.
		name:    "the last attempt asks to requeue",
		attempt: &workqueue.Attempt{Number: 5, Max: 5},
		agent: func(context.Context) error {
			return workqueue.InfrastructureError(workqueue.RequeueAfter(time.Minute), errors.New("model overloaded"))
		},
		opts: []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
	}, {
		// githubreconciler.Reconcile turns this into a requeue after the
		// metareconciler returns.
		name:    "the last attempt is rate limited",
		attempt: &workqueue.Attempt{Number: 5, Max: 5},
		agent: func(context.Context) error {
			return status.Error(codes.ResourceExhausted, "quota exceeded")
		},
		opts: []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
	}, {
		name:    "the last attempt queues keys",
		attempt: &workqueue.Attempt{Number: 5, Max: 5},
		agent: func(context.Context) error {
			return workqueue.QueueKeys(workqueue.QueueKey{Key: "https://github.com/owner/repo/issues/123"})
		},
		opts: []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
	}, {
		name:  "the dispatcher reports no attempt",
		agent: agentFails,
		opts:  []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
	}, {
		name:    "unbounded retries",
		attempt: &workqueue.Attempt{Number: 50},
		agent:   agentFails,
		opts:    []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
	}, {
		name:    "the note is not configured",
		attempt: &workqueue.Attempt{Number: 5, Max: 5},
		agent:   agentFails,
		opts:    []Option[*testRequest, *publishResult, struct{}]{withGiveUp()},
	}, {
		// Without the give-up marker the note would not answer the issue, so
		// its own event would start another round of attempts.
		name:    "no give-up comment to post under",
		attempt: &workqueue.Attempt{Number: 5, Max: 5},
		agent:   agentFails,
		opts:    []Option[*testRequest, *publishResult, struct{}]{withRetriesExhausted()},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newIssueHarness(t)
			ctx := t.Context()
			if tt.attempt != nil {
				ctx = workqueue.WithAttempt(ctx, *tt.attempt)
			}
			if err := h.reconcileErr(t, ctx, tt.agent, tt.opts...); err == nil {
				t.Fatal("reconcile: got = nil, want the agent's error")
			}
			notes := h.notes()
			if tt.wantNote == "" {
				if len(notes) != 0 {
					t.Errorf("notes: got = %q, want none", notes)
				}
				return
			}
			if len(notes) != 1 || !strings.HasSuffix(notes[0], tt.wantNote) {
				t.Errorf("notes: got = %q, want one ending %q", notes, tt.wantNote)
			}
		})
	}
}

// TestReconcileIssueRetriesExhaustedNoteAnswers proves the note answers the
// issue the way a give-up note does: its own event must not start another
// round of attempts, which would post the note again after every round, and a
// person's reply must start one.
func TestReconcileIssueRetriesExhaustedNoteAnswers(t *testing.T) {
	opts := []Option[*testRequest, *publishResult, struct{}]{withRetriesExhausted()}
	h := newIssueHarness(t)
	last := workqueue.WithAttempt(t.Context(), workqueue.Attempt{Number: 5, Max: 5})
	if err := h.reconcileErr(t, last, agentFails, append(opts, withGiveUp())...); err == nil {
		t.Fatal("reconcile: got = nil, want the agent's error")
	}
	h.step(t, "the note's own event", noChange, 1, opts...)
	h.addComment("filer", "please try again")
	h.step(t, "the reply runs the agent, which gives up", noChange, 2, opts...)
	if n := len(h.notes()); n != 1 {
		t.Errorf("bot's notes: got = %d, want = 1", n)
	}
}
