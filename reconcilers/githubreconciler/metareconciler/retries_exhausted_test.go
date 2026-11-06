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

// withRetriesExhaustedPR configures the PR note the tests look for.
func withRetriesExhaustedPR() Option[*testRequest, *publishResult, struct{}] {
	return WithRetriesExhaustedPRNote[*testRequest, *publishResult, struct{}](func(attempts int, _ string) string {
		return fmt.Sprintf("stopped updating the PR after %d attempts", attempts)
	})
}

// failingCheck is a head commit whose one check run failed, so the PR has a
// finding to iterate on.
const failingCheck = `[{"__typename":"CheckRun","databaseId":1,"name":"unit-tests","status":"COMPLETED","conclusion":"FAILURE"}]`

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
	}, {
		name:    "only the PR note is configured",
		attempt: &workqueue.Attempt{Number: 5, Max: 5},
		agent:   agentFails,
		opts:    []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhaustedPR()},
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

// TestReconcileIssueRetriesExhaustedPRNote proves that when the last attempt
// to update an open PR fails, the note goes on the PR, where the filer is
// waiting, and not on the issue; and that an attempt that will be retried
// posts nothing.
func TestReconcileIssueRetriesExhaustedPRNote(t *testing.T) {
	both := []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted(), withRetriesExhaustedPR()}
	tests := []struct {
		name       string
		mergeable  string
		checks     string
		attempt    workqueue.Attempt
		agent      func(context.Context) error
		opts       []Option[*testRequest, *publishResult, struct{}]
		wantPRNote string
	}{{
		name:       "the last attempt to fix a failing check fails",
		mergeable:  "MERGEABLE",
		checks:     failingCheck,
		attempt:    workqueue.Attempt{Number: 5, Max: 5},
		agent:      agentFails,
		opts:       both,
		wantPRNote: "stopped updating the PR after 5 attempts",
	}, {
		name:       "the last attempt to rebuild a conflicting PR fails",
		mergeable:  "CONFLICTING",
		checks:     `[]`,
		attempt:    workqueue.Attempt{Number: 5, Max: 5},
		agent:      agentFails,
		opts:       both,
		wantPRNote: "stopped updating the PR after 5 attempts",
	}, {
		name:      "an attempt with retries left fails",
		mergeable: "MERGEABLE",
		checks:    failingCheck,
		attempt:   workqueue.Attempt{Number: 4, Max: 5},
		agent:     agentFails,
		opts:      both,
	}, {
		name:      "the last attempt asks to requeue",
		mergeable: "MERGEABLE",
		checks:    failingCheck,
		attempt:   workqueue.Attempt{Number: 5, Max: 5},
		agent: func(context.Context) error {
			return workqueue.InfrastructureError(workqueue.RequeueAfter(time.Minute), errors.New("model overloaded"))
		},
		opts: both,
	}, {
		name:      "only the issue note is configured",
		mergeable: "MERGEABLE",
		checks:    failingCheck,
		attempt:   workqueue.Attempt{Number: 5, Max: 5},
		agent:     agentFails,
		opts:      []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhausted()},
	}, {
		name:      "no give-up comment to post under",
		mergeable: "MERGEABLE",
		checks:    failingCheck,
		attempt:   workqueue.Attempt{Number: 5, Max: 5},
		agent:     agentFails,
		opts:      []Option[*testRequest, *publishResult, struct{}]{withRetriesExhaustedPR()},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newIssueHarness(t)
			h.openPR(t, tt.mergeable, tt.checks)
			ctx := workqueue.WithAttempt(t.Context(), tt.attempt)
			if err := h.reconcileErr(t, ctx, tt.agent, tt.opts...); err == nil {
				t.Fatal("reconcile: got = nil, want the agent's error")
			}
			if h.agentCalls != 1 {
				t.Errorf("agent calls: got = %d, want = 1", h.agentCalls)
			}
			if notes := h.notes(); len(notes) != 0 {
				t.Errorf("issue notes: got = %q, want none", notes)
			}
			notes := h.prNotes()
			if tt.wantPRNote == "" {
				if len(notes) != 0 {
					t.Errorf("PR notes: got = %q, want none", notes)
				}
				return
			}
			if len(notes) != 1 || !strings.HasSuffix(notes[0], tt.wantPRNote) {
				t.Errorf("PR notes: got = %q, want one ending %q", notes, tt.wantPRNote)
			}
		})
	}
}

// TestReconcileIssueRetriesExhaustedPRNoteClears proves the PR note is one
// comment that a later failed round rewrites rather than repeats, and that it
// goes once the PR is green, so it never outlives the failure it reports.
func TestReconcileIssueRetriesExhaustedPRNoteClears(t *testing.T) {
	opts := []Option[*testRequest, *publishResult, struct{}]{withGiveUp(), withRetriesExhaustedPR()}
	h := newIssueHarness(t)
	h.openPR(t, "MERGEABLE", failingCheck)

	for _, attempts := range []int{5, 6} {
		ctx := workqueue.WithAttempt(t.Context(), workqueue.Attempt{Number: attempts, Max: attempts})
		if err := h.reconcileErr(t, ctx, agentFails, opts...); err == nil {
			t.Fatal("reconcile: got = nil, want the agent's error")
		}
	}
	notes := h.prNotes()
	if want := "stopped updating the PR after 6 attempts"; len(notes) != 1 || !strings.HasSuffix(notes[0], want) {
		t.Errorf("PR notes after two failed rounds: got = %q, want one ending %q", notes, want)
	}

	h.setPR("MERGEABLE", `[]`)
	h.reconcile(t, agentFails, opts...)
	if notes := h.prNotes(); len(notes) != 0 {
		t.Errorf("PR notes once the PR is green: got = %q, want none", notes)
	}
}
