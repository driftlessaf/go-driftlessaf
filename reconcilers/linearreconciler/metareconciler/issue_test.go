/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"slices"
	"testing"
)

// TestTransitionToNoDiff_OrdersCommentBeforeSave locks in the load-bearing
// invariant in transitionToNoDiff: UpsertBotComment must run before Save.
//
// The framework's linearreconciler.StateManager only persists the comment-
// tracking commentID alongside the state attachment on the next Save call.
// Saving first would persist commentID="" and leave the next reconcile
// posting a fresh comment instead of updating the existing one. This test
// will fail loudly if a future refactor swaps the call order.
func TestTransitionToNoDiff_OrdersCommentBeforeSave(t *testing.T) {
	f := newLinearStateFixture(t, `{}`)
	r := newReconcilerForFixture(t, f)

	if err := r.transitionToNoDiff(t.Context(), f.issue, "test note"); err != nil {
		t.Fatalf("transitionToNoDiff: %v", err)
	}

	got := f.getCallOrder()
	commentIdx := slices.Index(got, "comment")
	saveIdx := slices.Index(got, "save")
	if commentIdx < 0 {
		t.Fatalf("expected a comment call, sequence was %v", got)
	}
	if saveIdx < 0 {
		t.Fatalf("expected a save call, sequence was %v", got)
	}
	if commentIdx >= saveIdx {
		t.Errorf("UpsertBotComment must run BEFORE Save; sequence was %v", got)
	}
}
