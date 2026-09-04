/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"os/exec"
	"strings"
	"testing"
)

// TestGitCLIAutoGCDoesNotDetach confirms env() sets gc.autoDetach=false, so a
// triggered auto gc runs inside the git command instead of forking into the
// background. A detached gc can still be repacking objects after the command
// returns, which is what leaves a reopened go-git handle looking at a pack
// gc has already removed.
func TestGitCLIAutoGCDoesNotDetach(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	b := cliBackend{tokenSource: staticTokenSource("t")}
	env, err := b.env("")
	if err != nil {
		t.Fatalf("env: %v", err)
	}

	resolve := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "gc.autoDetach")
	resolve.Env = env
	out, err := resolve.Output()
	if err != nil {
		t.Fatalf("resolving gc.autoDetach: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "false" {
		t.Errorf("gc.autoDetach: got %q, want %q", got, "false")
	}
}
