/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"
	"go/token"
	"path/filepath"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/metapathreconciler"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/analysis/passes/modernize"
	"golang.org/x/tools/go/packages"
)

// goModernize implements metapathreconciler.Analyzer by running the Go modernize
// analysis passes on the Go packages at the given directory paths.
type goModernize struct{}

// Analyze runs the modernize analysis suite on the packages at the given
// directory paths and returns structured diagnostics.
func (a *goModernize) Analyze(ctx context.Context, wt *gogit.Worktree, paths []string, _ ...metapathreconciler.Diagnostic) ([]metapathreconciler.Diagnostic, error) {
	root := wt.Filesystem.Root()

	var diagnostics []metapathreconciler.Diagnostic
	for _, path := range paths {
		pkgDir := filepath.Join(root, path)
		clog.InfoContext(ctx, "Running Go modernize analyzer", "pkg_dir", pkgDir)

		// Load the package with full type information.
		pkgs, err := packages.Load(&packages.Config{
			Mode: packages.LoadAllSyntax,
			Dir:  pkgDir,
		}, ".")
		if err != nil {
			return nil, fmt.Errorf("load packages in %s: %w", path, err)
		}

		// Filter out packages with load errors (e.g., no Go files in
		// the directory, or the path is outside a Go module). These are
		// reported as per-package errors rather than top-level errors.
		pkgs = filterLoadable(pkgs)
		if len(pkgs) == 0 {
			clog.InfoContext(ctx, "Skipping path: no loadable Go packages", "pkg_dir", pkgDir)
			continue
		}

		// Run all modernize analyzers.
		graph, err := checker.Analyze(modernize.Suite, pkgs, nil)
		if err != nil {
			return nil, fmt.Errorf("run modernize analyzers in %s: %w", path, err)
		}

		// Collect diagnostics from all analyzers across all packages.
		for act := range graph.All() {
			for _, diag := range act.Diagnostics {
				d, ok := toDiagnostic(act.Package.Fset, root, diag.Pos, diag.Category, diag.Message)
				if !ok {
					continue
				}
				diagnostics = append(diagnostics, d)
			}
		}
	}

	clog.InfoContext(ctx, "Modernize analysis complete", "diagnostics", len(diagnostics))
	return diagnostics, nil
}

// toDiagnostic converts an analysis diagnostic position into a
// metapathreconciler.Diagnostic with a repo-relative path.
func toDiagnostic(fset *token.FileSet, repoRoot string, pos token.Pos, category, message string) (metapathreconciler.Diagnostic, bool) {
	position := fset.Position(pos)
	if !position.IsValid() {
		return metapathreconciler.Diagnostic{}, false
	}

	rel, err := filepath.Rel(repoRoot, position.Filename)
	if err != nil {
		return metapathreconciler.Diagnostic{}, false
	}

	return metapathreconciler.Diagnostic{
		Path:    rel,
		Line:    position.Line,
		Message: message,
		Rule:    category,
	}, true
}

// filterLoadable returns only those packages that loaded without errors.
//
// packages.Load reports errors differently depending on their origin:
//
//   - Infrastructure failures (directory doesn't exist, go binary not found,
//     type sizes can't be determined) are returned as a top-level error from
//     packages.Load itself. These are real problems that callers should propagate.
//
//   - "Not a Go package" conditions (no Go files in the directory, path is
//     outside a Go module) are NOT top-level errors. The golist driver in
//     x/tools catches these from go list's stderr and synthesizes incomplete
//     Package values with a populated Errors field. This happens because with
//     LoadAllSyntax the usesExportData check is true, so the friendlyErr path
//     that would otherwise surface a top-level error is skipped.
//
// This function filters out the second category so that non-Go paths passed
// to the analyzer produce zero diagnostics instead of causing failures.
func filterLoadable(pkgs []*packages.Package) []*packages.Package {
	var out []*packages.Package
	for _, p := range pkgs {
		if len(p.Errors) == 0 {
			out = append(out, p)
		}
	}
	return out
}
