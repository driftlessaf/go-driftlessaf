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

func TestScopeMarkActiveWithoutTouch(t *testing.T) {
	s := NewScope()
	s.MarkActive()
	if !s.Active() {
		t.Error("MarkActive did not activate scope")
	}
}

func TestScopeNilSafe(t *testing.T) {
	var s *Scope
	s.Touch("x")          // must not panic
	s.MarkActive()        // must not panic
	s.DeclareLockUpdate() // must not panic
	if s.Active() || s.Contains("x") || s.LockUpdateDeclared() {
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
