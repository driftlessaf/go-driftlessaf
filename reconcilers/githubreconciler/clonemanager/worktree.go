/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/internal/textedit"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/commitscope"
	gogit "github.com/go-git/go-git/v5"
)

// lineBound caps how far ReadFile scans for a newline when aligning a window
// to whole lines. A line longer than this is left cut.
const lineBound = 64 << 10

// recordTouch records the repo-relative paths an edit tool changed into the
// commit scope carried by the context, when one is present. It is a no-op when no
// scope is installed, so the callbacks work whether or not the commit guard is
// enabled.
func recordTouch(ctx context.Context, paths ...string) {
	if s, ok := commitscope.ScopeFromContext(ctx); ok {
		s.Touch(paths...)
	}
}

// WorktreeCallbacks creates callbacks.WorktreeCallbacks bound to a git worktree.
// All file operations are scoped to the worktree root directory.
//
// Every operation is resolved through an *os.Root opened on that directory.
// os.Root confines path resolution to the tree by construction: it walks a
// name component by component and refuses any step — including a symlink
// whose target escapes — that would leave the root, so a committed symlink
// (a git tree preserves symlink blobs as real symlinks on checkout) cannot
// redirect a callback to a file outside the worktree. A lexical check alone
// (filepath.Join/Clean/Rel) cannot catch this: it never asks the filesystem
// what a path component actually resolves to.
//
// The mutating callbacks write to the worktree on disk only; they never touch
// the git index. This keeps them safe for concurrent use within a single agent
// turn, which claudeexecutor requires of tool handlers (it dispatches a turn's
// tool calls in parallel). Staging is centralized in commitChanges, single-
// threaded, just before the commit. Per-write staging is unsafe here: go-git's
// Worktree.Add rewrites .git/index non-atomically (truncate in place, no lock),
// so two callbacks staging concurrently tear the index and a later read fails
// with an "invalid checksum" error.
//
// The mutating callbacks also record every path they change into the commit
// scope carried by the context, so the commit guard (when enabled) stages only
// those paths plus modifications to tracked files, and leaves artifacts a gate
// dropped into the tree uncommitted. Without the guard, commitChanges stages
// every worktree change (`git add -A`), which has two consequences a consumer
// cannot discover any other way:
//   - Any change in the worktree is committed, not only files written through
//     these callbacks. If a consumer's agent runs commands that drop artifacts
//     into the worktree, those artifacts land in the signed commit.
//   - Writes to paths matched by the target repo's .gitignore are silently
//     dropped from the commit: the callback succeeds and the file exists on
//     disk, but `git add -A` skips ignored paths, so the file never reaches
//     the commit tree.
func WorktreeCallbacks(wt *gogit.Worktree) callbacks.WorktreeCallbacks {
	rootDir := wt.Filesystem.Root()
	// Callback tool calls can run in parallel. Keep symlink checks and the
	// following operation together when another callback can change the tree.
	var mutationMu sync.RWMutex

	return callbacks.WorktreeCallbacks{
		ReadFile: func(_ context.Context, path string, offset int64, limit int) (callbacks.ReadResult, error) {
			mutationMu.RLock()
			defer mutationMu.RUnlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return callbacks.ReadResult{}, rootErr
			}
			defer root.Close()

			rel, err := relPath(path)
			if err != nil {
				return callbacks.ReadResult{}, err
			}

			if isBinaryFile(rel) {
				return callbacks.ReadResult{}, fmt.Errorf("file %q appears to be binary", path)
			}

			if err := checkNotSymlinkToGit(root, rel); err != nil {
				return callbacks.ReadResult{}, err
			}

			f, err := root.Open(rel)
			if err != nil {
				return callbacks.ReadResult{}, wrapRootErr(path, err)
			}
			defer f.Close()

			fi, err := f.Stat()
			if err != nil {
				return callbacks.ReadResult{}, err
			}
			fileSize := fi.Size()

			// Offset past EOF: empty content, no continuation.
			if offset >= fileSize {
				return callbacks.ReadResult{}, nil
			}

			// Determine the window, then align it to whole lines so the
			// agent never sees a first line with its indentation cut off.
			start, end := offset, fileSize
			if limit >= 0 && int64(limit) < fileSize-offset {
				end = offset + int64(limit)
			}
			if start > 0 {
				if start, err = textedit.LineStart(f, start, lineBound); err != nil {
					return callbacks.ReadResult{}, err
				}
			}
			if end < fileSize {
				if end, err = textedit.LineEnd(f, end, fileSize, lineBound); err != nil {
					return callbacks.ReadResult{}, err
				}
			}

			buf := make([]byte, end-start)
			n, err := f.ReadAt(buf, start)
			if err != nil && !errors.Is(err, io.EOF) {
				return callbacks.ReadResult{}, err
			}
			buf = buf[:n]

			// If the window ends before EOF, avoid splitting a UTF-8 character.
			if start+int64(n) < fileSize {
				buf = adjustUTF8Boundary(buf)
			}

			end = start + int64(len(buf))
			result := callbacks.ReadResult{
				Content:   string(buf),
				Offset:    start,
				Remaining: fileSize - end,
			}
			if end < fileSize {
				result.NextOffset = &end
			}
			return result, nil
		},

		WriteFile: func(ctx context.Context, path, content string, mode os.FileMode) error {
			mutationMu.Lock()
			defer mutationMu.Unlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return rootErr
			}
			defer root.Close()

			rel, err := writeRelPath(path)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, rel); err != nil {
				return err
			}
			if dir := filepath.Dir(rel); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return wrapRootErr(path, err)
				}
			}
			if err := root.WriteFile(rel, []byte(content), mode); err != nil {
				return wrapRootErr(path, err)
			}
			recordTouch(ctx, path)
			return nil
		},

		DeleteFile: func(ctx context.Context, path string) error {
			mutationMu.Lock()
			defer mutationMu.Unlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return rootErr
			}
			defer root.Close()

			rel, err := writeRelPath(path)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, rel); err != nil {
				return err
			}
			if err := root.Remove(rel); err != nil {
				return wrapRootErr(path, err)
			}
			recordTouch(ctx, path)
			return nil
		},

		MoveFile: func(ctx context.Context, src, dst string) error {
			mutationMu.Lock()
			defer mutationMu.Unlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return rootErr
			}
			defer root.Close()

			srcRel, err := writeRelPath(src)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, srcRel); err != nil {
				return err
			}
			dstRel, err := writeRelPath(dst)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, dstRel); err != nil {
				return err
			}
			if dir := filepath.Dir(dstRel); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return wrapRootErr(dst, err)
				}
			}
			if err := root.Rename(srcRel, dstRel); err != nil {
				return fmt.Errorf("move %q to %q: %w", src, dst, err)
			}
			recordTouch(ctx, src, dst)
			return nil
		},

		CopyFile: func(ctx context.Context, src, dst string) error {
			mutationMu.Lock()
			defer mutationMu.Unlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return rootErr
			}
			defer root.Close()

			srcRel, err := writeRelPath(src)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, srcRel); err != nil {
				return err
			}
			dstRel, err := writeRelPath(dst)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, dstRel); err != nil {
				return err
			}
			data, err := root.ReadFile(srcRel)
			if err != nil {
				return wrapRootErr(src, err)
			}
			srcInfo, err := root.Stat(srcRel)
			if err != nil {
				return wrapRootErr(src, err)
			}
			if dir := filepath.Dir(dstRel); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return wrapRootErr(dst, err)
				}
			}
			if err := root.WriteFile(dstRel, data, srcInfo.Mode()); err != nil {
				return wrapRootErr(dst, err)
			}
			recordTouch(ctx, dst)
			return nil
		},

		CreateSymlink: func(ctx context.Context, path, target string) error {
			mutationMu.Lock()
			defer mutationMu.Unlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return rootErr
			}
			defer root.Close()

			rel, err := writeRelPath(path)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, rel); err != nil {
				return err
			}
			if err := validateSymlinkTarget(rel, target); err != nil {
				return err
			}
			// Keep the target's .. components intact: os.Root resolves them
			// after following any symlink in the target path.
			targetPath := filepath.ToSlash(filepath.Dir(rel)) + "/" + filepath.ToSlash(target)
			if err := checkNotSymlinkToGit(root, targetPath); err != nil {
				return err
			}
			if dir := filepath.Dir(rel); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return wrapRootErr(path, err)
				}
			}
			if err := root.Symlink(target, rel); err != nil {
				return wrapRootErr(path, err)
			}
			recordTouch(ctx, path)
			return nil
		},

		Chmod: func(ctx context.Context, path string, mode os.FileMode) error {
			mutationMu.Lock()
			defer mutationMu.Unlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return rootErr
			}
			defer root.Close()

			rel, err := writeRelPath(path)
			if err != nil {
				return err
			}
			if err := checkNotSymlinkToGit(root, rel); err != nil {
				return err
			}
			if err := root.Chmod(rel, mode); err != nil {
				return wrapRootErr(path, err)
			}
			recordTouch(ctx, path)
			return nil
		},

		ListDirectory: func(_ context.Context, path, filter string, offset, limit int) (callbacks.ListResult, error) {
			mutationMu.RLock()
			defer mutationMu.RUnlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return callbacks.ListResult{}, rootErr
			}
			defer root.Close()

			rel, err := relPath(path)
			if err != nil {
				return callbacks.ListResult{}, err
			}

			dirFile, err := root.Open(rel)
			if err != nil {
				return callbacks.ListResult{}, wrapRootErr(path, err)
			}
			defer dirFile.Close()

			entries, err := dirFile.ReadDir(-1)
			if err != nil {
				return callbacks.ListResult{}, err
			}

			// Filter entries.
			filtered := make([]os.DirEntry, 0, len(entries))
			for _, e := range entries {
				if matchFilter(e.Name(), filter) {
					filtered = append(filtered, e)
				}
			}

			total := len(filtered)

			// Apply offset.
			if offset >= total {
				return callbacks.ListResult{}, nil
			}
			filtered = filtered[offset:]

			// Apply limit.
			var remaining int
			if len(filtered) > limit {
				remaining = len(filtered) - limit
				filtered = filtered[:limit]
			}

			result := callbacks.ListResult{
				Entries:   make([]callbacks.DirEntry, 0, len(filtered)),
				Remaining: remaining,
			}
			if remaining > 0 {
				nextOff := offset + limit
				result.NextOffset = &nextOff
			}

			for _, e := range filtered {
				de, err := buildDirEntry(root, rel, e)
				if err != nil {
					continue // Skip entries we can't stat.
				}
				result.Entries = append(result.Entries, de)
			}

			return result, nil
		},

		EditFile: func(ctx context.Context, path, oldString, newString string, replaceAll bool) (callbacks.EditResult, error) {
			mutationMu.Lock()
			defer mutationMu.Unlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return callbacks.EditResult{}, rootErr
			}
			defer root.Close()

			if len(oldString) == 0 {
				return callbacks.EditResult{}, errors.New("old_string must not be empty")
			}
			if len(oldString) > maxEditStringSize {
				return callbacks.EditResult{}, fmt.Errorf("old_string is %d bytes; use write_file for large replacements", len(oldString))
			}
			if len(newString) > maxEditStringSize {
				return callbacks.EditResult{}, fmt.Errorf("new_string is %d bytes; use write_file for large replacements", len(newString))
			}

			rel, err := writeRelPath(path)
			if err != nil {
				return callbacks.EditResult{}, err
			}
			if err := checkNotSymlinkToGit(root, rel); err != nil {
				return callbacks.EditResult{}, err
			}

			// Confine through root before streaming the edit: this is what
			// refuses a path that only escapes the worktree via a symlink.
			// The descriptor opened here is kept and reused for every
			// subsequent read of this file, and every write below goes
			// through root as well (a root-confined temp file, renamed into
			// place with root.Rename). A second, unconfined os.Open of the
			// lexical fullPath here would reopen a TOCTOU window: a path
			// component swapped to a symlink between the check and that
			// later open (or the final rename) could redirect the read, or
			// the write, outside the worktree.
			f, err := root.Open(rel)
			if err != nil {
				return callbacks.EditResult{}, wrapRootErr(path, err)
			}
			defer f.Close()

			// Plan: stream the file to find match offsets.
			offsets, err := planReplacements(f, []byte(oldString), replaceAll)
			if err != nil {
				return callbacks.EditResult{}, err
			}
			if len(offsets) == 0 {
				result, err := editIgnoringIndentation(root, rel, f, oldString, newString)
				if err != nil {
					return callbacks.EditResult{}, err
				}
				recordTouch(ctx, path)
				return result, nil
			}

			// Execute: stream the file again, replacing at recorded offsets.
			if err := executeReplacements(root, rel, f, offsets, len(oldString), []byte(newString)); err != nil {
				return callbacks.EditResult{}, err
			}

			recordTouch(ctx, path)
			return callbacks.EditResult{Replacements: len(offsets)}, nil
		},

		SearchCodebase: func(_ context.Context, searchPath, pattern, filter string, offset, limit int) (callbacks.SearchResult, error) {
			mutationMu.RLock()
			defer mutationMu.RUnlock()
			root, rootErr := os.OpenRoot(rootDir)
			if rootErr != nil {
				return callbacks.SearchResult{}, rootErr
			}
			defer root.Close()

			searchRel, err := relPath(searchPath)
			if err != nil {
				return callbacks.SearchResult{}, err
			}
			if err := checkNotSymlinkToGit(root, searchRel); err != nil {
				return callbacks.SearchResult{}, err
			}

			// Preserve the error for a search path that leaves the root.
			// The walk and each file read below also use root, so a path
			// changed by another process cannot redirect them outside it.
			dirFile, err := root.Open(searchRel)
			if err != nil {
				return callbacks.SearchResult{}, wrapRootErr(searchPath, err)
			}
			dirFile.Close()

			re, err := regexp.Compile(pattern)
			if err != nil {
				return callbacks.SearchResult{}, fmt.Errorf("invalid pattern: %w", err)
			}

			// Collect offset+limit+1 matches in a single pass so we can
			// stop walking as soon as we know the page is full.
			need := offset + limit + 1 // +1 to detect whether more remain
			var allMatches []callbacks.Match

			err = fs.WalkDir(root.FS(), filepath.ToSlash(searchRel), func(filePath string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil // Skip inaccessible entries.
				}

				// Never follow a symlink encountered during the walk: a
				// symlink committed elsewhere in the tree could otherwise
				// redirect a read to a different file in the worktree.
				if d.Type()&os.ModeSymlink != 0 {
					return nil
				}

				if d.IsDir() {
					if filePath != "." && strings.HasPrefix(d.Name(), ".") {
						return fs.SkipDir
					}
					return nil
				}

				if isBinaryFile(filePath) {
					return nil
				}

				if !matchFilter(d.Name(), filter) {
					return nil
				}

				data, err := root.ReadFile(filePath)
				if err != nil {
					return nil // Skip unreadable files.
				}

				for _, loc := range re.FindAllIndex(data, -1) {
					allMatches = append(allMatches, callbacks.Match{
						Path:   filepath.ToSlash(filePath),
						Offset: int64(loc[0]),
						Length: loc[1] - loc[0],
					})
					if len(allMatches) >= need {
						return fs.SkipAll
					}
				}

				return nil
			})
			if err != nil {
				return callbacks.SearchResult{}, err
			}

			// Apply offset.
			if offset >= len(allMatches) {
				return callbacks.SearchResult{}, nil
			}
			allMatches = allMatches[offset:]

			// Apply limit.
			hasMore := len(allMatches) > limit
			if hasMore {
				allMatches = allMatches[:limit]
			}

			result := callbacks.SearchResult{
				Matches: allMatches,
				HasMore: hasMore,
			}
			if hasMore {
				nextOff := offset + limit
				result.NextOffset = &nextOff
			}

			return result, nil
		},
	}
}

// ErrConfigSymlink marks a package config rejected because its path contains
// a symlink. The checkout must change before retrying that path can succeed.
var ErrConfigSymlink = errors.New("config path contains a symlink")

// SafeConfigPath validates that repoRelPath, resolved against workingTree,
// stays within the worktree and does not resolve through a symlink, then
// returns the absolute path a caller may hand to a third-party parser (such
// as melange's config.ParseConfiguration) that opens a plain filesystem path
// itself rather than going through WorktreeCallbacks.
//
// Such callers read outside os.Root's confinement, so a committed symlink at
// repoRelPath — a git tree preserves a symlink blob as a real symlink on
// checkout — could otherwise redirect the read to a file outside the
// worktree, or to another tracked file such as .git/config. Reconciliation
// paths (update-bot's reconciler, analyzer, and levelup) call this before
// parsing a monitored package's config so a legitimate config is never itself
// a symlink.
func SafeConfigPath(workingTree, repoRelPath string) (string, error) {
	root, err := os.OpenRoot(workingTree)
	if err != nil {
		return "", fmt.Errorf("opening worktree root: %w", err)
	}
	defer root.Close()

	rel, err := relPath(repoRelPath)
	if err != nil {
		return "", err
	}

	// Check every component of rel, not only the leaf. root.Lstat follows
	// symlinks in every path component except the final one, so a
	// committed intermediate symlink such as "pkg -> .git" would let
	// root.Lstat("pkg/config") resolve straight through "pkg" and report
	// on the file it points at instead — passing a leaf-only check for
	// the very component that redirects the read.
	if rel != "." {
		parts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
		for i, part := range parts {
			partial := filepath.Join(parts[:i+1]...)

			fi, err := root.Lstat(partial)
			if err != nil {
				return "", wrapRootErr(repoRelPath, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("path %q: component %q is a symlink: %w", repoRelPath, part, ErrConfigSymlink)
			}
		}
	}

	return filepath.Join(workingTree, rel), nil
}

// relPath cleans path into a name safe to pass to a *os.Root method for this
// worktree: relative to the root, with no leading separator, and rejecting
// any lexical climb above it. os.Root itself refuses, at the point each
// method is called, any resolution — including through a symlink — that
// would leave the root, which is what actually confines the callbacks: a
// worktree checkout preserves a committed symlink blob as a real symlink on
// disk, and a purely lexical check here cannot see where such a symlink
// really points.
func relPath(path string) (string, error) {
	rel := filepath.Clean(path)
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	if rel == "" {
		rel = "."
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes worktree", path)
	}
	return rel, nil
}

// writeRelPath is relPath for a mutating callback: it also refuses any path
// with a ".git" path component. Writing, deleting, moving, or chmod-ing under
// .git (HEAD, index, config, hooks, info/exclude) would rewrite the
// repository's state, git config, or hooks — outside the tree the tools are
// meant to touch, and a way to defeat commit scoping. Read callbacks use
// relPath, so reading the worktree is unaffected.
func writeRelPath(path string) (string, error) {
	rel, err := relPath(path)
	if err != nil {
		return "", err
	}
	if hasGitComponent(rel) {
		return "", fmt.Errorf("path %q resolves into the .git directory", path)
	}
	return rel, nil
}

// wrapRootErr adds the caller-supplied path to an error from a *os.Root
// method. os.Root reports any out-of-root resolution — including one that
// only manifests through a symlink — as a "path escapes from parent" error;
// that phrasing is preserved but called out explicitly as a worktree escape,
// since it is the one case callers most need to recognize.
func wrapRootErr(path string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "path escapes from parent") {
		return fmt.Errorf("path %q escapes the worktree (possibly via a symlink): %w", path, err)
	}
	return fmt.Errorf("path %q: %w", path, err)
}

// hasGitComponent reports whether a slash- or OS-separated relative path contains
// an exact ".git" path segment (case-insensitive, since a case-insensitive
// filesystem treats ".GIT" as ".git"). A file merely named like ".gitignore" or a
// directory named ".github" is not a match: only the whole ".git" segment is.
func hasGitComponent(rel string) bool {
	for seg := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}

// validateSymlinkTarget checks that a symlink target will not escape the
// worktree root. Absolute targets are always rejected. Relative targets are
// resolved lexically from the symlink's own worktree-relative parent
// directory to verify they stay within root; os.Root additionally enforces
// this — and rejects an absolute target outright — at the point the symlink
// is later followed, so this is a fast, clear-error first check rather than
// the sole guard.
func validateSymlinkTarget(linkRel, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("symlink target %q is absolute; only relative targets are allowed", target)
	}

	// Resolve the target relative to the symlink's parent directory, both
	// expressed relative to the worktree root.
	linkDir := filepath.Dir(linkRel)
	effectivePath := filepath.Clean(filepath.Join(linkDir, target))

	if effectivePath == ".." || strings.HasPrefix(effectivePath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("symlink target %q resolves outside worktree", target)
	}
	if hasGitComponent(effectivePath) {
		return fmt.Errorf("symlink target %q resolves into the .git directory", target)
	}
	return nil
}

// checkNotSymlinkToGit refuses paths that resolve through a symlink into
// .git. It checks every component, including the components of each symlink
// target; Lstat on a complete path follows any symlink before the leaf.
func checkNotSymlinkToGit(root *os.Root, rel string) error {
	return checkSymlinkChain(root, rel)
}

// maxSymlinkChainDepth bounds how many hops checkSymlinkChain follows before
// giving up, guarding against a cycle such as a -> b, b -> a.
const maxSymlinkChainDepth = 40

// checkSymlinkChain resolves path components in the same order as os.Root.
// In particular, a ".." in a symlink target applies after any preceding
// symlink is followed. A missing component is left to the caller to report.
func checkSymlinkChain(root *os.Root, path string) error {
	pending := strings.Split(filepath.ToSlash(path), "/")
	var resolved []string
	symlinks := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return fmt.Errorf("path %q resolves outside worktree", path)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}

		// Direct reads of .git are allowed, but a symlink must never
		// redirect a read or write into it.
		if symlinks > 0 && strings.EqualFold(part, ".git") {
			return fmt.Errorf("path %q resolves into the .git directory", path)
		}
		prefix := filepath.Join(append(resolved, part)...)
		fi, err := root.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			resolved = append(resolved, part)
			continue
		}
		if err != nil {
			return wrapRootErr(path, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = append(resolved, part)
			continue
		}

		symlinks++
		if symlinks > maxSymlinkChainDepth {
			return fmt.Errorf("path %q: too many levels of symbolic links", path)
		}
		target, err := root.Readlink(prefix)
		if err != nil {
			return fmt.Errorf("reading symlink %q: %w", prefix, err)
		}
		if err := validateSymlinkTarget(prefix, target); err != nil {
			return fmt.Errorf("path %q already exists as a symlink: %w", prefix, err)
		}
		pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
	}
	return nil
}

// adjustUTF8Boundary trims trailing bytes that form an incomplete UTF-8 sequence.
func adjustUTF8Boundary(buf []byte) []byte {
	if utf8.Valid(buf) {
		return buf
	}
	// Walk backward up to 4 bytes (max UTF-8 length) to find the start of the
	// incomplete sequence.
	for i := len(buf) - 1; i >= 0 && i >= len(buf)-4; i-- {
		if utf8.RuneStart(buf[i]) {
			r, size := utf8.DecodeRune(buf[i:])
			if r == utf8.RuneError && size <= 1 {
				return buf[:i]
			}
			break
		}
	}
	return buf
}

// matchFilter checks if a filename matches the given filter.
// An empty filter matches everything. A filter containing * is treated as a
// glob pattern (only * wildcards are supported). Otherwise it is an exact match.
func matchFilter(name, filter string) bool {
	if filter == "" {
		return true
	}
	if strings.Contains(filter, "*") {
		matched, _ := filepath.Match(filter, name)
		return matched
	}
	return name == filter
}

// buildDirEntry creates a callbacks.DirEntry from an os.DirEntry, resolving
// metadata for it through root so a symlinked entry cannot redirect the
// Lstat/Readlink calls outside the worktree.
func buildDirEntry(root *os.Root, dirRel string, e os.DirEntry) (callbacks.DirEntry, error) {
	entryRel := filepath.Join(dirRel, e.Name())

	// Use Lstat so symlinks are not followed.
	fi, err := root.Lstat(entryRel)
	if err != nil {
		return callbacks.DirEntry{}, err
	}

	de := callbacks.DirEntry{
		Name: e.Name(),
		Mode: fi.Mode().Perm(),
		Size: fi.Size(),
	}

	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		de.Type = "symlink"
		de.Size = 0
		if target, err := root.Readlink(entryRel); err == nil {
			de.Target = target
		}
	case fi.IsDir():
		de.Type = "directory"
		de.Size = 0
	default:
		de.Type = "file"
	}

	return de, nil
}

// isBinaryFile checks if a file is binary based on its extension.
func isBinaryFile(path string) bool {
	_, isBinary := binaryExts[strings.ToLower(filepath.Ext(path))]
	return isBinary
}

var binaryExts = map[string]struct{}{
	".exe": {}, ".dll": {}, ".so": {}, ".dylib": {},
	".zip": {}, ".tar": {}, ".gz": {}, ".bz2": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".ico": {},
	".pdf": {}, ".doc": {}, ".docx": {},
	".bin": {}, ".dat": {},
}

// maxEditStringSize is the maximum allowed size for old_string and new_string
// in EditFile. Edits larger than this should use WriteFile instead.
const maxEditStringSize = 32 * 1024

// planReplacements streams the file through a sliding window and collects byte
// offsets of all non-overlapping occurrences of pattern. When replaceAll is
// false, it returns an error as soon as a second match is found.
//
// The window uses an overlap of patLen-1 bytes between chunks so that matches
// spanning a chunk boundary are never missed:
//
//	Chunk N read:
//	┌──────────────────────────────────────────┐
//	│              buf (up to 1 MB)            │
//	│  searched ──────────────►  overlap       │
//	│                            (patLen-1)    │
//	└──────────────────────────────────────────┘
//	                              │
//	         slide: discard ◄─────┘ keep
//	                              ▼
//	Chunk N+1 read:
//	┌───────────┬──────────────────────────────┐
//	│  overlap  │     new data from Read()     │
//	│ (patLen-1)│                               │
//	└───────────┴──────────────────────────────┘
//
// A match requires patLen bytes, so the overlap (patLen-1 bytes) alone can
// never contain a complete match — it only serves as a prefix for matches that
// straddle the chunk boundary:
//
//	                    chunk boundary
//	                         │
//	  ┌──────────────────────┼──────────────────────┐
//	  │  ...XYZAB            │ CDrest...             │
//	  └──────────────────────┼──────────────────────┘
//	          ▲               │
//	          └── pattern "ABCD" starts in chunk N
//	              but only 2 bytes fit ("AB")
//
//	After slide, overlap = "AB" (last patLen-1 = 3 bytes would be "ZAB"
//	for patLen=4). Next chunk prepends this overlap:
//
//	  ┌───────┬─────────────────────┐
//	  │ ZAB   │ CDrest...           │
//	  └───────┴─────────────────────┘
//	    ▲
//	    └── bytes.Index finds "ABCD" starting at offset 1
func planReplacements(f *os.File, pattern []byte, replaceAll bool) ([]int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	patLen := len(pattern)
	const bufSize = 1 << 20

	var offsets []int64
	var filePos int64

	buf := make([]byte, 0, bufSize)
	chunk := make([]byte, bufSize)

	for {
		n, readErr := f.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}

		// Search for all complete matches in buf. bytes.Index only returns
		// matches where the entire pattern fits, so partial matches at the
		// end of buf are naturally deferred to the next iteration via the
		// overlap window.
		searchFrom := 0
		for {
			idx := bytes.Index(buf[searchFrom:], pattern)
			if idx == -1 {
				break
			}
			offsets = append(offsets, filePos+int64(searchFrom+idx))
			searchFrom += idx + patLen

			if !replaceAll && len(offsets) > 1 {
				return nil, fmt.Errorf("old_string appears more than once in file (at byte offsets %d and %d); include more surrounding context to make it unique, or set replace_all to true", offsets[0], offsets[1])
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}

		// Slide: discard fully-searched bytes, keep the last patLen-1 bytes
		// as overlap so boundary-spanning matches are found on the next read.
		overlap := min(patLen-1, len(buf))
		discard := len(buf) - overlap
		if discard > 0 {
			filePos += int64(discard)
			copy(buf[:overlap], buf[discard:])
			buf = buf[:overlap]
		}
	}

	return offsets, nil
}

// editIgnoringIndentation is the fallback for an EditFile whose old_string has
// no exact match: it accepts a unique region that differs from old_string only
// by a constant indentation shift and shifts new_string the same way. Its
// errors begin with "old_string not found in file".
func editIgnoringIndentation(root *os.Root, rel string, f *os.File, oldString, newString string) (callbacks.EditResult, error) {
	m, err := matchIgnoringIndentation(f, oldString)
	if err != nil {
		return callbacks.EditResult{}, err
	}
	if err := executeReplacements(root, rel, f, []int64{m.Start}, int(m.End-m.Start), []byte(textedit.ShiftIndentation(newString, m.Shift))); err != nil {
		return callbacks.EditResult{}, err
	}
	result := callbacks.EditResult{Replacements: 1}
	if m.Shift.Whitespace != "" {
		result.Note = fmt.Sprintf("old_string matched after adjusting indentation (%s); new_string was shifted the same way", m.Shift)
	}
	return result, nil
}

// matchIgnoringIndentation scans the already-open, root-confined descriptor
// for the streaming scan, rather than reopening the path unconfined.
func matchIgnoringIndentation(f *os.File, oldString string) (textedit.Match, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return textedit.Match{}, err
	}
	return textedit.MatchIgnoringIndentation(f, oldString)
}

// executeReplacements streams the file and writes a new version with the
// pattern at each recorded offset replaced by newBytes. It writes to a
// temporary file in the same directory and atomically renames it into place.
//
// For each recorded offset, three steps occur:
//
//	Source file:
//	┌──────────┬──────────┬──────────┬──────────┬─────────┐
//	│ prefix₁  │  old₁    │ prefix₂  │  old₂    │  tail   │
//	└──────────┴──────────┴──────────┴──────────┴─────────┘
//	     │          │           │          │          │
//	     ▼          ▼           ▼          ▼          ▼
//	Temp file:
//	┌──────────┬──────────┬──────────┬──────────┬─────────┐
//	│ prefix₁  │  new₁    │ prefix₂  │  new₂    │  tail   │
//	└──────────┴──────────┴──────────┴──────────┴─────────┘
//
// Prefixes are streamed via io.CopyN (bounded memory), old patterns are
// skipped in the source, and new replacements are written directly. The
// tail after the last match is streamed via io.Copy.
// src is the already-open, root-confined descriptor for rel (opened once by
// EditFile via root.Open); the temp file is created and renamed through root
// as well, so no step of this function reopens the lexical path unconfined —
// closing the TOCTOU window a second os.Open/os.CreateTemp/os.Rename on the
// path would otherwise leave between EditFile's confinement check and the
// actual read/write.
func executeReplacements(root *os.Root, rel string, src *os.File, offsets []int64, oldLen int, newBytes []byte) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}

	fi, err := src.Stat()
	if err != nil {
		return err
	}

	tmpRel, tmp, err := createTempInRoot(root, filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		root.Remove(tmpRel) //nolint:errcheck // best-effort cleanup; a failed rename already reports the real error
	}()

	var pos int64
	skipLen := int64(oldLen)

	for _, offset := range offsets {
		// Copy bytes before this match.
		if offset > pos {
			if _, err := io.CopyN(tmp, src, offset-pos); err != nil {
				return err
			}
		}
		// Skip the old pattern in source.
		if _, err := io.CopyN(io.Discard, src, skipLen); err != nil {
			return err
		}
		// Write the replacement.
		if _, err := tmp.Write(newBytes); err != nil {
			return err
		}
		pos = offset + skipLen
	}

	// Copy remaining bytes after the last match.
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}

	if err := tmp.Chmod(fi.Mode()); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return root.Rename(tmpRel, rel)
}

// createTempInRoot creates a new, exclusively-owned temp file inside dirRel
// (a worktree-relative directory) confined through root, mirroring
// os.CreateTemp's collision-retry behavior without ever forming an
// unconfined absolute path.
func createTempInRoot(root *os.Root, dirRel string) (string, *os.File, error) {
	for range 10000 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", nil, err
		}
		name := ".edit-" + hex.EncodeToString(suffix[:])
		rel := name
		if dirRel != "" && dirRel != "." {
			rel = filepath.Join(dirRel, name)
		}
		f, err := root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return rel, f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("failed to create temp file: too many collisions")
}
