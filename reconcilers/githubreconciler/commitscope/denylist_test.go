/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import "testing"

func TestDeniedRules(t *testing.T) {
	dl := DefaultDenylist()
	tests := []struct {
		name     string
		change   Change
		wantDeny bool
	}{
		{
			name:     "plain source file kept",
			change:   Change{Path: "internal/foo/bar.go", Type: Added, TrackedAtBase: false},
			wantDeny: false,
		},
		{
			name:     "tracked go.sum modification kept",
			change:   Change{Path: "go.sum", Type: Modified, TrackedAtBase: true},
			wantDeny: false,
		},
		{
			name:     "untracked binary dropped",
			change:   Change{Path: "cmd/tool/tool", Type: Added, IsBinary: true},
			wantDeny: true,
		},
		{
			name:     "terraform provider dir dropped",
			change:   Change{Path: "infra/.terraform/providers/registry/x", Type: Added},
			wantDeny: true,
		},
		{
			name:     "tfstate dropped",
			change:   Change{Path: "infra/terraform.tfstate", Type: Added},
			wantDeny: true,
		},
		{
			name:     "tfstate backup dropped",
			change:   Change{Path: "infra/terraform.tfstate.backup", Type: Added},
			wantDeny: true,
		},
		{
			name:     "tflint cache dropped",
			change:   Change{Path: "infra/.tflint.d/plugin", Type: Added},
			wantDeny: true,
		},
		{
			name:     "pycache dropped",
			change:   Change{Path: "scripts/__pycache__/x.cpython-314.pyc", Type: Added},
			wantDeny: true,
		},
		{
			name:     "pyc dropped",
			change:   Change{Path: "scripts/x.pyc", Type: Added},
			wantDeny: true,
		},
		{
			name:     "venv dropped",
			change:   Change{Path: ".venv/bin/python", Type: Added},
			wantDeny: true,
		},
		{
			name:     "node_modules dropped",
			change:   Change{Path: "web/node_modules/left-pad/index.js", Type: Added},
			wantDeny: true,
		},
		{
			name:     "compiled test binary dropped",
			change:   Change{Path: "internal/foo/foo.test", Type: Added},
			wantDeny: true,
		},
		{
			name:     "profile output dropped",
			change:   Change{Path: "cpu.prof", Type: Added},
			wantDeny: true,
		},
		{
			name:     "out file dropped",
			change:   Change{Path: "mem.out", Type: Added},
			wantDeny: true,
		},
		{
			name:     "coverage output dropped",
			change:   Change{Path: "coverage.txt", Type: Added},
			wantDeny: true,
		},
		{
			name:     "log dropped",
			change:   Change{Path: "run.log", Type: Added},
			wantDeny: true,
		},
		{
			name:     "sarif dropped",
			change:   Change{Path: "results.sarif", Type: Added},
			wantDeny: true,
		},
		{
			name:     "swap file dropped",
			change:   Change{Path: "main.go.swp", Type: Added},
			wantDeny: true,
		},
		{
			name:     "editor backup dropped",
			change:   Change{Path: "main.go~", Type: Added},
			wantDeny: true,
		},
		{
			name:     "ds store dropped",
			change:   Change{Path: "docs/.DS_Store", Type: Added},
			wantDeny: true,
		},
		{
			name:     "semgrep cache dropped",
			change:   Change{Path: ".semgrep/cache", Type: Added},
			wantDeny: true,
		},
		{
			name:     "untracked go.work dropped",
			change:   Change{Path: "go.work", Type: Added, TrackedAtBase: false},
			wantDeny: true,
		},
		{
			name:     "tracked go.work kept",
			change:   Change{Path: "go.work", Type: Modified, TrackedAtBase: true},
			wantDeny: false,
		},
		{
			name:     "untracked dist dir dropped",
			change:   Change{Path: "dist/app", Type: Added, TrackedAtBase: false},
			wantDeny: true,
		},
		{
			name:     "tracked dist file kept",
			change:   Change{Path: "dist/keep.txt", Type: Modified, TrackedAtBase: true},
			wantDeny: false,
		},
		{
			name:     "symlink replaced by file refused",
			change:   Change{Path: ".terraform.lock.hcl", Type: Modified, TrackedAtBase: true, BaseIsSymlink: true, CurrentIsSymlink: false},
			wantDeny: true,
		},
		{
			name:     "lock file without intent refused",
			change:   Change{Path: ".terraform.lock.hcl", Type: Modified, TrackedAtBase: true},
			wantDeny: true,
		},
		{
			name:     "oversize untracked file dropped",
			change:   Change{Path: "big.json", Type: Added, Size: defaultMaxFileSize + 1},
			wantDeny: true,
		},
		{
			name:     "oversize tracked file kept",
			change:   Change{Path: "big.json", Type: Modified, TrackedAtBase: true, Size: defaultMaxFileSize + 1},
			wantDeny: false,
		},
		{
			name:     "tracked binary modification kept",
			change:   Change{Path: "assets/logo.png", Type: Modified, TrackedAtBase: true, IsBinary: true, BaseIsBinary: true},
			wantDeny: false,
		},
		{
			name:     "path escaping worktree dropped",
			change:   Change{Path: "../evil", Type: Added},
			wantDeny: true,
		},
		{
			name:     "deletion of tracked file kept",
			change:   Change{Path: "old.go", Type: Deleted, TrackedAtBase: true},
			wantDeny: false,
		},
		{
			name:     "deletion of tracked artifact-named file kept",
			change:   Change{Path: "logs/run.log", Type: Deleted, TrackedAtBase: true},
			wantDeny: false,
		},
		{
			name:     "deletion of tracked lock file kept",
			change:   Change{Path: ".terraform.lock.hcl", Type: Deleted, TrackedAtBase: true},
			wantDeny: false,
		},
		{
			name:     "coverage-prefixed source file kept",
			change:   Change{Path: "internal/coverage_report.go", Type: Added},
			wantDeny: false,
		},
		{
			name:     "coverage dot go source kept",
			change:   Change{Path: "internal/coverage.go", Type: Added},
			wantDeny: false,
		},
		{
			name:     "coverage html output dropped",
			change:   Change{Path: "coverage.html", Type: Added},
			wantDeny: true,
		},
		{
			name:     "nested lock file refused",
			change:   Change{Path: "env/prod/.terraform.lock.hcl", Type: Modified, TrackedAtBase: true},
			wantDeny: true,
		},
		{
			name:     "lock-like backup not treated as lock",
			change:   Change{Path: "foo.terraform.lock.hcl.bak", Type: Added},
			wantDeny: false,
		},
		{
			name:     "build as substring of dir segment kept",
			change:   Change{Path: "pkg/builder/x.go", Type: Added},
			wantDeny: false,
		},
		{
			name:     "build as prefix of dir segment kept",
			change:   Change{Path: "rebuild/x.go", Type: Added},
			wantDeny: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotDeny, reason := dl.Denied(tc.change)
			if gotDeny != tc.wantDeny {
				t.Errorf("Denied(%+v): got deny = %v (%q), want %v", tc.change, gotDeny, reason, tc.wantDeny)
			}
			if gotDeny && reason == "" {
				t.Errorf("Denied(%+v): denied with empty reason", tc.change)
			}
		})
	}
}

func TestDeniedLockUpdateAllowed(t *testing.T) {
	dl := DefaultDenylist()
	dl.AllowLockUpdate = true
	// A regular-file lock modification is allowed once the run declares intent.
	c := Change{Path: ".terraform.lock.hcl", Type: Modified, TrackedAtBase: true}
	if deny, reason := dl.Denied(c); deny {
		t.Errorf("Denied lock with intent: got deny = true (%q), want false", reason)
	}
	// A symlink replaced by a regular file is refused even with intent.
	c.BaseIsSymlink = true
	if deny, _ := dl.Denied(c); !deny {
		t.Errorf("Denied de-symlinked lock with intent: got deny = false, want true")
	}
}

func TestDeniedSizeDisabled(t *testing.T) {
	dl := Denylist{MaxFileSize: 0}
	c := Change{Path: "big.json", Type: Added, Size: 1 << 30}
	if deny, reason := dl.Denied(c); deny {
		t.Errorf("Denied huge file with size check off: got deny = true (%q), want false", reason)
	}
}
