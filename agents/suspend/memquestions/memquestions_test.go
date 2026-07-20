/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package memquestions_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/suspend/memquestions"
)

func TestNonceBinding(t *testing.T) {
	ctx := t.Context()
	s := memquestions.New()

	// No pending question: Pending and Answer are false, Provide reports false.
	if _, ok, _ := s.Pending(ctx, "k"); ok {
		t.Fatal("Pending(no question): want false")
	}
	if _, ok, _ := s.Answer(ctx, "k"); ok {
		t.Fatal("Answer(no question): want false")
	}
	if _, ok := s.Provide(ctx, "k", "x"); ok {
		t.Fatal("Provide(no question): want false")
	}

	// Ask q1, answer it.
	if err := s.Ask(ctx, "k", suspend.Question{ID: "nonce-1", Key: "k", Prompt: "q1"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if q, ok, _ := s.Pending(ctx, "k"); !ok || q.ID != "nonce-1" {
		t.Fatalf("Pending after Ask: ok=%v q=%+v, want nonce-1", ok, q)
	}
	if _, ok := s.Provide(ctx, "k", "answer-1"); !ok {
		t.Fatal("Provide q1: want ok")
	}
	ans, ok, _ := s.Answer(ctx, "k")
	if !ok || ans.Text != "answer-1" || ans.QuestionID != "nonce-1" {
		t.Fatalf("Answer q1: ok=%v ans=%+v", ok, ans)
	}

	// A new Ask (fresh pause) supersedes q1's answer: Answer is false again.
	if err := s.Ask(ctx, "k", suspend.Question{ID: "nonce-2", Key: "k", Prompt: "q2"}); err != nil {
		t.Fatalf("Ask q2: %v", err)
	}
	if _, ok, _ := s.Answer(ctx, "k"); ok {
		t.Fatal("Answer after re-Ask: stale answer must not surface")
	}

	// Consume with a mismatched nonce is a no-op; matching nonce clears.
	if err := s.Consume(ctx, "k", "nonce-1"); err != nil {
		t.Fatalf("Consume(stale): %v", err)
	}
	if _, ok := s.Provide(ctx, "k", "answer-2"); !ok {
		t.Fatal("Provide q2: want ok (entry must still exist)")
	}
	if err := s.Consume(ctx, "k", "nonce-2"); err != nil {
		t.Fatalf("Consume(current): %v", err)
	}
	if _, ok, _ := s.Answer(ctx, "k"); ok {
		t.Fatal("Answer after Consume: want false")
	}
}
