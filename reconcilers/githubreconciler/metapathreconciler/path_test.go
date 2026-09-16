/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

func TestPathExists(t *testing.T) {
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mod", "pkg"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "mod", "go.mod"), []byte("module example.com/mod\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{name: "existing file", rel: "mod/go.mod", want: true},
		{name: "existing directory", rel: "mod/pkg", want: true},
		{name: "removed module", rel: "gone/go.mod", want: false},
		{name: "missing file in existing directory", rel: "mod/pkg/main.go", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pathExists(wt, tc.rel)
			if err != nil {
				t.Fatalf("pathExists(%q): unexpected error: %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("pathExists(%q): got = %v, want = %v", tc.rel, got, tc.want)
			}
		})
	}
}
