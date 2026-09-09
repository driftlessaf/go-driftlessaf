/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestImportBoundary asserts that this package's production source imports
// only the standard library, so a new Surface adapter binds without
// changing this package. It is an allowlist because no list of forbidden
// names covers every host client.
func TestImportBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !withinBoundary(path) {
				t.Errorf("%s imports %q; the request-status model must stay host-independent, so host clients belong in a Surface adapter", name, path)
			}
		}
	}
}

// allowedImports names the non-stdlib imports this package may use. Empty
// by design: widening the boundary is an edit here, reviewed as such.
var allowedImports = map[string]struct{}{}

// withinBoundary reports whether path is in allowedImports or is a standard
// library path, whose first element is never a domain and so has no dot.
func withinBoundary(path string) bool {
	if _, ok := allowedImports[path]; ok {
		return true
	}
	root, _, _ := strings.Cut(path, "/")
	return !strings.Contains(root, ".")
}

// TestWithinBoundary pins that host SDKs the check does not name by hand
// are still rejected.
func TestWithinBoundary(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "fmt", want: true},
		{path: "net/url", want: true},
		{path: "go/parser", want: true},
		{path: "github.com/shurcooL/githubv4"},
		{path: "github.com/google/go-github/v66/github"},
		{path: "gitlab.com/gitlab-org/api/client-go"},
		{path: "chainguard.dev/driftlessaf/changemanager"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := withinBoundary(tt.path); got != tt.want {
				t.Fatalf("withinBoundary(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
