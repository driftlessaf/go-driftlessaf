/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/commitscope"
)

func ExampleDefaultDenylist() {
	dl := commitscope.DefaultDenylist()
	deny, reason := dl.Denied(commitscope.Change{Path: "cmd/app/app.test", Type: commitscope.Added})
	fmt.Println(deny, reason)
	// Output: true artifact file (.test)
}

func ExamplePlan() {
	changes := []commitscope.Change{
		{Path: "internal/fix.go", Type: commitscope.Added, Intended: true},
		{Path: "go.sum", Type: commitscope.Modified, Intended: true, TrackedAtBase: true},
		{Path: "profile.prof", Type: commitscope.Added, Intended: true},
		{Path: "stray.txt", Type: commitscope.Added, Intended: false},
	}
	res := commitscope.Plan(changes, commitscope.DefaultDenylist())
	for _, c := range res.Staged {
		fmt.Println("stage:", c.Path)
	}
	for _, d := range res.Dropped {
		fmt.Println("drop:", d.Path, "-", d.Reason)
	}
	// Output:
	// stage: internal/fix.go
	// stage: go.sum
	// drop: profile.prof - artifact file (.prof)
	// drop: stray.txt - not produced by an edit tool or finalizer
}

func ExampleScope() {
	scope := commitscope.NewScope()
	scope.Touch("internal/fix.go")
	scope.DeclareLockUpdate()
	fmt.Println(scope.Active(), scope.Contains("internal/fix.go"), scope.LockUpdateDeclared())
	// Output: true true true
}

func ExampleScope_context() {
	scope := commitscope.NewScope()
	ctx := commitscope.WithScope(context.Background(), scope)

	// An edit-tool callback recovers the scope from its context and records a
	// write, marking the scope active.
	if s, ok := commitscope.ScopeFromContext(ctx); ok {
		s.Touch("internal/fix.go")
	}
	fmt.Println(scope.Active())
	// Output: true
}

func ExampleChangeType() {
	fmt.Println(commitscope.Added, commitscope.Modified, commitscope.Deleted)
	// Output: added modified deleted
}

// ExampleStage documents the worktree-facing entry points. Stage and the
// Snapshot helpers operate on a git worktree and are wired into the commit path
// by the clone manager rather than called directly by a bot, so this example
// only references them.
func ExampleStage() {
	_ = commitscope.Stage
	_ = commitscope.TakeSnapshot
	_ = (*commitscope.Snapshot).Restore
	_ = commitscope.NewScope().MarkActive
	// Output:
}
