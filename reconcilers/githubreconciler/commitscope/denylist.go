/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// defaultMaxFileSize is the per-file ceiling the default denylist enforces.
const defaultMaxFileSize = 1 << 20 // 1 MiB

// terraformLockName is the Terraform dependency lock file. A plain
// `terraform init` rewrites it and, when it is a symlink in the source tree,
// replaces it with a regular file, so the guard refuses it unless the run
// declares a lock-file update.
const terraformLockName = ".terraform.lock.hcl"

// workflowDirs are the repository-root directories GitHub reads to decide what
// runs on a pull request: the workflow definitions, and the conventional home
// of the local actions they call. A pull request from a branch in the
// repository runs an edited workflow before any review, so a consumer that
// commits on an untrusted request refuses changes here unless the run declares
// a workflow edit. Only the root directories count: GitHub does not run a
// `.github/workflows/` nested deeper in the tree. A local action outside
// `.github/actions/` is caught by its manifest instead, see actionManifests.
var workflowDirs = []string{".github/workflows", ".github/actions"}

// actionManifests are the file names of a GitHub action's manifest. A workflow
// can call a local action at any path (`uses: ./ci/setup`), so the manifest is
// refused wherever it sits, not only under `.github/actions/`.
var actionManifests = map[string]struct{}{"action.yml": {}, "action.yaml": {}}

// WorkflowEditDeniedReason is the reason [Denylist.Denied] gives for a change
// the workflow rule refused. The rule is checked last, so a workflow path that
// another rule refuses (binary content, size) carries that rule's reason. A
// consumer that reports drops matches on this reason (see
// [Dropped.DeniedWorkflowEdit]) rather than on the path: only such a drop is
// one a workflow-edit declaration would have committed.
const WorkflowEditDeniedReason = "GitHub workflow or action (no workflow edit declared for this run)"

// ignoredDirs are path segments whose entire subtree is an artifact directory.
var ignoredDirs = map[string]struct{}{
	".terraform":   {},
	".tflint.d":    {},
	"__pycache__":  {},
	".venv":        {},
	"node_modules": {},
}

// untrackedDirs are artifact directories refused only when untracked at base, so
// a repository that legitimately tracks a dist/ or build/ tree is unaffected.
var untrackedDirs = map[string]struct{}{
	"dist":  {},
	"build": {},
}

// artifactSuffixes are file-name suffixes of build and tooling artifacts.
var artifactSuffixes = []string{".pyc", ".test", ".out", ".prof", ".log", ".sarif", ".swp"}

// untrackedFiles are file names refused only when untracked at base.
var untrackedFiles = map[string]struct{}{
	"go.work":     {},
	"go.work.sum": {},
}

// Denylist configures which changes are refused at commit time, independent of
// whether an edit tool or a finalizer produced them.
type Denylist struct {
	// MaxFileSize refuses any single changed file larger than this many bytes,
	// unless the path was already tracked at the base revision. Zero disables
	// the size check.
	MaxFileSize int64
	// AllowLockUpdate permits committing a Terraform dependency lock file. It is
	// off by default: a lock file usually changes as a side effect of an init
	// run, not because the change intends to update dependencies.
	AllowLockUpdate bool
	// DenyWorkflowEdits refuses additions and modifications to the files GitHub
	// reads to decide what runs: anything under the repository's
	// `.github/workflows/` and `.github/actions/` directories, and an action
	// manifest anywhere in the tree (see [IsWorkflowPath]), unless the run
	// declares a workflow edit with [Scope.DeclareWorkflowEdit]. It is off by
	// default, so a consumer whose agent legitimately maintains workflows is
	// unaffected; a consumer that acts on untrusted requests opts in, because an
	// edited workflow runs in CI the moment its pull request opens.
	//
	// Like every denylist rule it is applied by [Stage], which the clone manager
	// runs only for an active [Scope]. On its whole-tree fallback (an inactive
	// scope) the clone manager fails closed instead: a worktree holding a
	// workflow change refuses the commit rather than carrying the change
	// unchecked (its ErrUnguardedWorkflowEdit).
	DenyWorkflowEdits bool
}

// DefaultDenylist returns the standard denylist: a 1 MiB per-file ceiling and no
// lock-file updates.
func DefaultDenylist() Denylist { return Denylist{MaxFileSize: defaultMaxFileSize} }

// Denied reports whether a change must be kept out of the commit and a short
// reason. It never reads intent: a denied path is refused even when an edit tool
// wrote it. Adding a rule here only ever refuses more, never fewer, changes.
func (d Denylist) Denied(c Change) (bool, string) {
	if !fs.ValidPath(c.Path) {
		return true, "path is not confined to the worktree"
	}

	// A deletion removes content already tracked at the base; it can never
	// introduce an artifact, so an intended deletion of a confined path is always
	// allowed (this covers deleting a tracked file whose name matches an artifact
	// pattern, such as a stale foo.test or a lock file). Unintended deletions
	// never reach here: Plan drops them before consulting the denylist.
	if c.Type == Deleted {
		return false, ""
	}

	// A tool that replaced a tracked symlink with a regular file (a plain
	// `terraform init` de-symlinking a shared lock file). Refuse the type change
	// so the committed tree keeps the symlink.
	if c.BaseIsSymlink && !c.CurrentIsSymlink {
		return true, "refuses a symlink replaced by a regular file"
	}

	base := path.Base(c.Path)
	segments := strings.Split(c.Path, "/")

	if base == terraformLockName && !d.AllowLockUpdate {
		return true, "Terraform lock file (no lock-file update declared for this run)"
	}

	for _, seg := range segments {
		if _, ok := ignoredDirs[seg]; ok {
			return true, fmt.Sprintf("artifact directory %q", seg)
		}
		if strings.HasPrefix(seg, ".semgrep") {
			return true, "semgrep cache or output"
		}
	}

	if base == ".DS_Store" {
		return true, "operating-system metadata file"
	}
	for _, suffix := range artifactSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true, fmt.Sprintf("artifact file (%s)", suffix)
		}
	}
	if strings.HasSuffix(base, "~") {
		return true, "editor backup file"
	}
	if strings.Contains(base, ".tfstate") {
		return true, "Terraform state file"
	}
	// Coverage output such as coverage.out, coverage.txt, or coverage.html. A dot
	// must follow the prefix, and a Go source file is never coverage output, so a
	// legitimately named source file such as coverage_report.go or coverage.go is
	// not refused.
	if strings.HasPrefix(base, "coverage.") && !strings.HasSuffix(base, ".go") {
		return true, "coverage output"
	}

	if !c.TrackedAtBase {
		if _, ok := untrackedFiles[base]; ok {
			return true, fmt.Sprintf("untracked workspace file %q", base)
		}
		for _, seg := range segments {
			if _, ok := untrackedDirs[seg]; ok {
				return true, fmt.Sprintf("untracked build directory %q", seg)
			}
		}
	}

	if d.MaxFileSize > 0 && c.Size > d.MaxFileSize && !c.TrackedAtBase {
		return true, fmt.Sprintf("file size %d exceeds limit %d bytes", c.Size, d.MaxFileSize)
	}

	// A binary already tracked as binary at the base is an in-place update of an
	// asset the repository commits; new binary content is refused.
	trackedBinary := c.TrackedAtBase && c.BaseIsBinary
	if c.IsBinary && !trackedBinary {
		return true, "binary content"
	}

	// Last on purpose: a workflow path that another rule refuses carries that
	// rule's reason, so a consumer that reports this reason as lifted by a
	// workflow-edit declaration never blames the declaration for a drop it
	// would not have lifted.
	if d.DenyWorkflowEdits && IsWorkflowPath(c.Path) {
		return true, WorkflowEditDeniedReason
	}

	return false, ""
}

// IsWorkflowPath reports whether a repo-relative, slash-separated path is one
// GitHub reads to decide what runs: `.github/workflows` or `.github/actions` at
// the repository root or anything beneath them, or an action manifest
// (`action.yml`, `action.yaml`) anywhere in the tree, since a workflow can call
// a local action at any path (`uses: ./ci/setup`). It is the predicate behind
// [Denylist.DenyWorkflowEdits], exported so a consumer can tell those drops
// apart from artifact drops when it reports them.
//
// It does not follow what CI executes. A script, Makefile or Dockerfile that a
// workflow or action runs is an ordinary repository file to this rule and is
// reviewed like any other change; covering those would mean refusing most of
// the tree.
func IsWorkflowPath(p string) bool {
	for _, dir := range workflowDirs {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	_, manifest := actionManifests[path.Base(p)]
	return manifest
}
