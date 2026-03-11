/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// HistoryCallbacks creates callbacks.HistoryCallbacks bound to a git repository,
// showing changes between baseCommit and the current HEAD.
func HistoryCallbacks(repo *gogit.Repository, baseCommit plumbing.Hash) callbacks.HistoryCallbacks {
	return callbacks.HistoryCallbacks{
		ListCommits: func(ctx context.Context, offset, limit int) (callbacks.CommitListResult, error) {
			all, err := collectCommits(repo, baseCommit)
			if err != nil {
				return callbacks.CommitListResult{}, err
			}

			total := len(all)
			if offset >= total {
				return callbacks.CommitListResult{Total: total}, nil
			}
			page := all[offset:min(offset+limit, total)]

			infos := make([]callbacks.CommitInfo, 0, len(page))
			for _, c := range page {
				files, err := commitFiles(c)
				if err != nil {
					return callbacks.CommitListResult{}, fmt.Errorf("diff for commit %s: %w", c.Hash.String()[:7], err)
				}
				infos = append(infos, callbacks.CommitInfo{
					SHA:     c.Hash.String()[:7],
					Message: c.Message,
					Files:   files,
				})
			}

			result := callbacks.CommitListResult{
				Commits: infos,
				Total:   total,
			}
			if offset+len(infos) < total {
				next := offset + len(infos)
				result.NextOffset = &next
			}
			return result, nil
		},
		GetFileDiff: func(ctx context.Context, path, start, end string, offset int64, limit int) (callbacks.FileDiffResult, error) {
			fromCommit, err := resolveCommitOrBase(repo, baseCommit, start)
			if err != nil {
				return callbacks.FileDiffResult{}, fmt.Errorf("resolve start: %w", err)
			}
			toCommit, err := resolveCommitOrHead(repo, end)
			if err != nil {
				return callbacks.FileDiffResult{}, fmt.Errorf("resolve end: %w", err)
			}

			fp, err := filePatchBetween(fromCommit, toCommit, path)
			if err != nil {
				return callbacks.FileDiffResult{}, fmt.Errorf("compute diff: %w", err)
			}

			var buf bytes.Buffer
			if err := encodeFilePatch(&buf, fp); err != nil {
				return callbacks.FileDiffResult{}, fmt.Errorf("encode diff: %w", err)
			}

			return paginateDiff(buf.String(), offset, limit), nil
		},
	}
}

// ResolveBaseCommit walks commitCount commits from HEAD and returns the first
// parent of the oldest commit found. This derives the actual merge-base from
// the PR branch's own ancestry, avoiding reliance on the base branch tip SHA
// (which may not be present in a shallow clone).
//
// This assumes a linear commit history (no merge commits on the PR branch),
// which is the case for bot-created PRs. For merge commits, ParentHashes[0]
// is the first-parent which may not be the intended base.
//
// The caller must ensure the clone has sufficient depth (commitCount+1) via
// WithCommitDepth before calling this function.
//
// Returns plumbing.ZeroHash if the oldest commit has no parents (root commit).
func ResolveBaseCommit(repo *gogit.Repository, commitCount int) (plumbing.Hash, error) {
	head, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("get HEAD: %w", err)
	}

	iter, err := repo.Log(&gogit.LogOptions{
		From:  head.Hash(),
		Order: gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("log: %w", err)
	}
	defer iter.Close()

	var last *object.Commit
	for range commitCount {
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("walk commit: %w", err)
		}
		last = c
	}

	if last == nil || last.NumParents() == 0 {
		return plumbing.ZeroHash, nil
	}
	return last.ParentHashes[0], nil
}

// collectCommits returns all commits from HEAD down to (but not including)
// baseCommit in reverse chronological order.
func collectCommits(repo *gogit.Repository, baseCommit plumbing.Hash) ([]*object.Commit, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("get HEAD: %w", err)
	}

	iter, err := repo.Log(&gogit.LogOptions{
		From:  head.Hash(),
		Order: gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}
	defer iter.Close()

	var commits []*object.Commit
	err = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == baseCommit {
			return storer.ErrStop
		}
		commits = append(commits, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk commits: %w", err)
	}
	return commits, nil
}

// commitFiles computes the changed files for a single commit by diffing
// against its first parent (or an empty tree for root commits).
func commitFiles(c *object.Commit) ([]callbacks.CommitFile, error) {
	var patch *object.Patch
	if c.NumParents() > 0 {
		parent, err := c.Parent(0)
		if err != nil {
			return nil, fmt.Errorf("get parent: %w", err)
		}
		patch, err = parent.Patch(c)
		if err != nil {
			return nil, fmt.Errorf("compute patch: %w", err)
		}
	} else {
		toTree, err := c.Tree()
		if err != nil {
			return nil, fmt.Errorf("get tree: %w", err)
		}
		patch, err = (&object.Tree{}).Patch(toTree)
		if err != nil {
			return nil, fmt.Errorf("compute patch: %w", err)
		}
	}

	return filesFromPatch(patch)
}

// filePatchBetween computes the diff between two commits and returns the
// file patch for the specified path. If from is nil, diffs against an empty tree.
func filePatchBetween(from, to *object.Commit, path string) (diff.FilePatch, error) {
	var patch *object.Patch
	var err error
	if from == nil {
		toTree, err := to.Tree()
		if err != nil {
			return nil, fmt.Errorf("get tree: %w", err)
		}
		patch, err = (&object.Tree{}).Patch(toTree)
		if err != nil {
			return nil, fmt.Errorf("compute patch: %w", err)
		}
	} else {
		patch, err = from.Patch(to)
		if err != nil {
			return nil, fmt.Errorf("compute patch: %w", err)
		}
	}

	for _, fp := range patch.FilePatches() {
		fpFrom, fpTo := fp.Files()
		if (fpTo != nil && fpTo.Path() == path) || (fpFrom != nil && fpFrom.Path() == path) {
			return fp, nil
		}
	}
	return nil, fmt.Errorf("file %q not changed in the specified range", path)
}

// filesFromPatch extracts file change information from a patch.
func filesFromPatch(patch *object.Patch) ([]callbacks.CommitFile, error) {
	filePatches := patch.FilePatches()
	files := make([]callbacks.CommitFile, 0, len(filePatches))

	for _, fp := range filePatches {
		from, to := fp.Files()

		var cf callbacks.CommitFile
		switch {
		case from == nil && to != nil:
			cf.Path = to.Path()
			cf.Type = "added"
		case from != nil && to == nil:
			cf.Path = from.Path()
			cf.Type = "deleted"
		case from != nil && to != nil && from.Path() != to.Path():
			cf.Path = to.Path()
			cf.OldPath = from.Path()
			cf.Type = "renamed"
		default:
			cf.Path = to.Path()
			cf.Type = "modified"
		}

		size, err := filePatchSize(fp)
		if err != nil {
			return nil, fmt.Errorf("encode diff for %s: %w", cf.Path, err)
		}
		cf.DiffSize = size
		files = append(files, cf)
	}
	return files, nil
}

// filePatchSize computes the byte size of a single file's unified diff.
func filePatchSize(fp diff.FilePatch) (int, error) {
	var buf bytes.Buffer
	if err := encodeFilePatch(&buf, fp); err != nil {
		return 0, err
	}
	return buf.Len(), nil
}

// resolveCommitOrBase resolves a short SHA to a commit object. If sha is empty,
// returns the commit at baseCommit. Returns nil for the base commit to
// signal "diff against empty tree" when baseCommit itself is the zero hash.
func resolveCommitOrBase(repo *gogit.Repository, baseCommit plumbing.Hash, sha string) (*object.Commit, error) {
	if sha == "" {
		if baseCommit.IsZero() {
			return nil, nil
		}
		return repo.CommitObject(baseCommit)
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(sha))
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", sha, err)
	}
	return repo.CommitObject(*hash)
}

// resolveCommitOrHead resolves a short SHA to a commit, or HEAD if empty.
func resolveCommitOrHead(repo *gogit.Repository, sha string) (*object.Commit, error) {
	if sha == "" {
		head, err := repo.Head()
		if err != nil {
			return nil, fmt.Errorf("get HEAD: %w", err)
		}
		return repo.CommitObject(head.Hash())
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(sha))
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", sha, err)
	}
	return repo.CommitObject(*hash)
}

// encodeFilePatch writes a single file's unified diff to w.
func encodeFilePatch(w io.Writer, fp diff.FilePatch) error {
	e := diff.NewUnifiedEncoder(w, 3)
	return e.Encode(&singleFilePatch{fp: fp})
}

// singleFilePatch wraps a single FilePatch to satisfy the diff.Patch interface.
type singleFilePatch struct {
	fp diff.FilePatch
}

func (s *singleFilePatch) FilePatches() []diff.FilePatch { return []diff.FilePatch{s.fp} }
func (s *singleFilePatch) Message() string               { return "" }

// paginateDiff applies byte offset/limit pagination to diff text.
func paginateDiff(fullDiff string, offset int64, limit int) callbacks.FileDiffResult {
	total := int64(len(fullDiff))

	if offset >= total {
		return callbacks.FileDiffResult{}
	}

	end := min(offset+int64(limit), total)
	remaining := total - end
	result := callbacks.FileDiffResult{
		Diff:      fullDiff[offset:end],
		Remaining: remaining,
	}
	if remaining > 0 {
		result.NextOffset = &end
	}
	return result
}
