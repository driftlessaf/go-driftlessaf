/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"strconv"
	"sync"
	"testing"
)

func TestScopeTouchAndContains(t *testing.T) {
	s := NewScope()
	if s.Active() {
		t.Fatal("new scope reports active")
	}
	s.Touch("pkg/a.go", "./pkg/b.go", "")
	if !s.Active() {
		t.Error("scope not active after Touch")
	}
	for _, want := range []string{"pkg/a.go", "pkg/b.go"} {
		if !s.Contains(want) {
			t.Errorf("Contains(%q) = false, want true", want)
		}
	}
	if s.Contains("pkg/c.go") {
		t.Error("Contains(untouched) = true, want false")
	}
	// Normalization: a slash-prefixed or dot-relative form matches.
	if !s.Contains("./pkg/a.go") {
		t.Error("Contains(dot-relative) = false, want true")
	}
}

func TestScopeLockUpdate(t *testing.T) {
	s := NewScope()
	if s.LockUpdateDeclared() {
		t.Fatal("new scope declares lock update")
	}
	s.DeclareLockUpdate()
	if !s.LockUpdateDeclared() {
		t.Error("LockUpdateDeclared = false after DeclareLockUpdate")
	}
}

func TestScopeWorkflowEdit(t *testing.T) {
	s := NewScope()
	if s.WorkflowEditDeclared() {
		t.Fatal("new scope declares workflow edit")
	}
	s.DeclareWorkflowEdit()
	if !s.WorkflowEditDeclared() {
		t.Error("WorkflowEditDeclared = false after DeclareWorkflowEdit")
	}
	// The declarations are independent: a workflow edit does not lift the
	// lock-file rule, and declaring one does not activate the scope.
	if s.LockUpdateDeclared() {
		t.Error("DeclareWorkflowEdit also declared a lock update")
	}
	if s.Active() {
		t.Error("DeclareWorkflowEdit activated the scope")
	}
}

func TestScopeDropped(t *testing.T) {
	s := NewScope()
	if got := s.Dropped(); len(got) != 0 {
		t.Fatalf("new scope Dropped = %v, want empty", got)
	}
	drops := []Dropped{{Path: ".github/workflows/ci.yaml", Reason: "workflow"}, {Path: "a.out", Reason: "artifact"}}
	s.recordDropped(drops)
	got := s.Dropped()
	if len(got) != 2 || got[0] != drops[0] || got[1] != drops[1] {
		t.Errorf("Dropped = %v, want %v", got, drops)
	}
	// The record is a copy in both directions: mutating the input or the
	// output does not change what the scope holds.
	drops[0].Path = "changed"
	got[1].Path = "changed"
	if again := s.Dropped(); again[0].Path != ".github/workflows/ci.yaml" || again[1].Path != "a.out" {
		t.Errorf("Dropped after external mutation = %v, want the recorded paths", again)
	}
	// A later record replaces the earlier one, so a scope reused across Stage
	// calls reports the latest run.
	s.recordDropped(nil)
	if got := s.Dropped(); len(got) != 0 {
		t.Errorf("Dropped after recording nil = %v, want empty", got)
	}
}

func TestScopeMarkActiveWithoutTouch(t *testing.T) {
	s := NewScope()
	s.MarkActive()
	if !s.Active() {
		t.Error("MarkActive did not activate scope")
	}
}

func TestScopeNilSafe(t *testing.T) {
	var s *Scope
	s.Touch("x")                            // must not panic
	s.MarkActive()                          // must not panic
	s.DeclareLockUpdate()                   // must not panic
	s.DeclareWorkflowEdit()                 // must not panic
	s.recordDropped([]Dropped{{Path: "x"}}) // must not panic
	if s.Active() || s.Contains("x") || s.LockUpdateDeclared() || s.WorkflowEditDeclared() || s.Dropped() != nil {
		t.Error("nil scope reported a non-zero value")
	}
}

func TestScopeContext(t *testing.T) {
	ctx := t.Context()
	if _, ok := ScopeFromContext(ctx); ok {
		t.Error("empty context reported a scope")
	}
	s := NewScope()
	ctx = WithScope(ctx, s)
	got, ok := ScopeFromContext(ctx)
	if !ok || got != s {
		t.Errorf("ScopeFromContext: got (%v, %v), want (%v, true)", got, ok, s)
	}

	var nilScope *Scope
	if _, ok := ScopeFromContext(WithScope(t.Context(), nilScope)); ok {
		t.Error("context carrying a nil scope reported present")
	}
}

// TestScopeConcurrentTouch runs many Touch calls in parallel; -race proves the
// mutex covers the shared map, matching the parallel edit-tool dispatch.
func TestScopeConcurrentTouch(t *testing.T) {
	s := NewScope()
	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			s.Touch("pkg/file" + strconv.Itoa(i) + ".go")
		}()
	}
	wg.Wait()
	for i := range n {
		if !s.Contains("pkg/file" + strconv.Itoa(i) + ".go") {
			t.Fatalf("missing concurrently touched path %d", i)
		}
	}
}
