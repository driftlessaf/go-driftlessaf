/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"context"
	"path"
	"path/filepath"
	"sync"
)

// Scope records the repo-relative paths an edit tool intentionally changed
// during one run, plus whether the run declared a Terraform lock-file update.
//
// Scope is safe for concurrent use: an executor may dispatch a turn's edit-tool
// callbacks in parallel, and each records into the same Scope.
//
// A nil *Scope is usable: every method is a no-op or reports the zero value, so
// callback code can record unconditionally.
type Scope struct {
	mu         sync.Mutex
	active     bool
	lockUpdate bool
	touched    map[string]struct{}
}

// NewScope returns an empty Scope.
func NewScope() *Scope {
	return &Scope{touched: make(map[string]struct{})}
}

// MarkActive records that the managed edit-tool callback layer ran this run. The
// commit guard uses intent staging only for an active scope; an inactive scope
// (a run that wrote through some other path) is left to the caller's fallback.
func (s *Scope) MarkActive() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.active = true
	s.mu.Unlock()
}

// Active reports whether the edit-tool callback layer ran this run.
func (s *Scope) Active() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Touch records repo-relative paths an edit tool wrote, deleted, moved, or
// copied. It marks the scope active. Empty paths are ignored.
func (s *Scope) Touch(paths ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = true
	for _, p := range paths {
		if np := normalizePath(p); np != "" {
			s.touched[np] = struct{}{}
		}
	}
}

// DeclareLockUpdate records that this run intentionally updates a Terraform
// dependency lock file, so the guard does not refuse it.
func (s *Scope) DeclareLockUpdate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lockUpdate = true
	s.mu.Unlock()
}

// LockUpdateDeclared reports whether the run declared a lock-file update.
func (s *Scope) LockUpdateDeclared() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockUpdate
}

// Contains reports whether an edit tool touched the given path this run.
func (s *Scope) Contains(p string) bool {
	if s == nil {
		return false
	}
	np := normalizePath(p)
	if np == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.touched[np]
	return ok
}

// normalizePath renders a path repo-relative and slash-separated, so a path from
// an edit tool and the same path from git status compare equal.
func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	p = path.Clean(filepath.ToSlash(p))
	if p == "." {
		return ""
	}
	return p
}

// scopeKey is the context key under which a Scope rides.
type scopeKey struct{}

// WithScope returns a context carrying the Scope, so edit-tool callbacks that
// receive only a context can record their writes.
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFromContext returns the Scope carried by the context. The boolean reports
// whether one was present; a nil Scope reports false.
func ScopeFromContext(ctx context.Context) (*Scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(*Scope)
	if s == nil {
		return nil, false
	}
	return s, ok
}
