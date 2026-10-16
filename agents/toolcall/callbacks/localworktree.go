/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"chainguard.dev/driftlessaf/internal/textedit"
)

// lineBound caps how far ReadFile scans for a newline when aligning a window
// to whole lines. A line longer than this is left cut.
const lineBound = 64 << 10

const (
	maxReadBytes       = 1 << 20
	maxSearchFileBytes = 4 << 20
	maxSearchMatches   = 1000
	maxSearchOffset    = 10000
)

var errSearchFileTooLarge = errors.New("file exceeds search byte limit")

// LocalWorktree returns a WorktreeCallbacks backed by r, an os.Root that
// sandboxes all file operations within a single directory tree. Path traversal
// outside the root is prevented by the os.Root implementation.
// ReadFile caps a window at 1 MiB plus at most 64 KiB of line alignment on
// either side, including when limit is negative. SearchCodebase caps pages at
// 1000 matches, rejects offsets above 10000, and errors on files over 4 MiB;
// narrow the path or filter when a search exceeds these limits.
func LocalWorktree(r *os.Root) WorktreeCallbacks {
	return WorktreeCallbacks{
		ReadFile: func(ctx context.Context, path string, offset int64, limit int) (ReadResult, error) {
			if err := checkOffset(offset); err != nil {
				return ReadResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return ReadResult{}, err
			}
			f, err := openRegularFile(r, path)
			if err != nil {
				return ReadResult{}, err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return ReadResult{}, err
			}
			size := info.Size()
			if offset >= size {
				return ReadResult{}, nil
			}
			if limit < 0 || limit > maxReadBytes {
				limit = maxReadBytes
			}
			start, end := offset, offset+min(int64(limit), size-offset)
			// Align the window to whole lines so the agent never sees a
			// first line with its indentation cut off.
			if start > 0 {
				if start, err = textedit.LineStart(f, start, lineBound); err != nil {
					return ReadResult{}, err
				}
			}
			if end < size {
				if end, err = textedit.LineEnd(f, end, size, lineBound); err != nil {
					return ReadResult{}, err
				}
			}
			data := make([]byte, end-start)
			if _, err := f.ReadAt(data, start); err != nil {
				return ReadResult{}, err
			}
			result := ReadResult{Content: string(data), Offset: start}
			if end < size {
				result.NextOffset = &end
				result.Remaining = size - end
			}
			return result, nil
		},

		WriteFile: func(_ context.Context, path, content string, mode os.FileMode) error {
			if err := r.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return r.WriteFile(path, []byte(content), mode)
		},

		EditFile: func(_ context.Context, path, oldString, newString string, replaceAll bool) (EditResult, error) {
			data, err := r.ReadFile(path)
			if err != nil {
				return EditResult{}, err
			}
			content := string(data)
			count := strings.Count(content, oldString)
			if !replaceAll && count > 1 {
				return EditResult{}, fmt.Errorf("string found %d times in %q; use replace_all=true to replace all occurrences", count, path)
			}
			var (
				updated string
				result  EditResult
			)
			switch {
			case count == 0:
				// No exact match: accept a region that differs only by a
				// constant indentation shift and shift newString the same way.
				m, err := textedit.MatchIgnoringIndentation(bytes.NewReader(data), oldString)
				if err != nil {
					return EditResult{}, err
				}
				updated = content[:m.Start] + textedit.ShiftIndentation(newString, m.Shift) + content[m.End:]
				result = EditResult{Replacements: 1, Note: indentationNote(m.Shift)}
			case replaceAll:
				updated = strings.ReplaceAll(content, oldString, newString)
				result = EditResult{Replacements: count}
			default:
				updated = strings.Replace(content, oldString, newString, 1)
				result = EditResult{Replacements: 1}
			}
			info, err := r.Stat(path)
			if err != nil {
				return EditResult{}, err
			}
			if err := r.WriteFile(path, []byte(updated), info.Mode()); err != nil {
				return EditResult{}, err
			}
			return result, nil
		},

		DeleteFile: func(_ context.Context, path string) error {
			return r.Remove(path)
		},

		MoveFile: func(_ context.Context, src, dst string) error {
			return r.Rename(src, dst)
		},

		CopyFile: func(_ context.Context, src, dst string) error {
			data, err := r.ReadFile(src)
			if err != nil {
				return err
			}
			info, err := r.Stat(src)
			if err != nil {
				return err
			}
			return r.WriteFile(dst, data, info.Mode())
		},

		CreateSymlink: func(_ context.Context, path, target string) error {
			return r.Symlink(target, path)
		},

		Chmod: func(_ context.Context, path string, mode os.FileMode) error {
			return r.Chmod(path, mode)
		},

		ListDirectory: func(_ context.Context, path, filter string, offset, limit int) (ListResult, error) {
			if err := checkOffset(int64(offset)); err != nil {
				return ListResult{}, err
			}
			entries, err := fs.ReadDir(r.FS(), path)
			if err != nil {
				return ListResult{}, err
			}
			var filtered []DirEntry
			for _, e := range entries {
				if filter != "" {
					matched, matchErr := filepath.Match(filter, e.Name())
					if matchErr != nil || !matched {
						continue
					}
				}
				info, infoErr := e.Info()
				if infoErr != nil {
					continue
				}
				entryType := "file"
				switch {
				case e.IsDir():
					entryType = "directory"
				case e.Type()&os.ModeSymlink != 0:
					entryType = "symlink"
				}
				target := ""
				if entryType == "symlink" {
					target, _ = r.Readlink(filepath.Join(path, e.Name()))
				}
				filtered = append(filtered, DirEntry{
					Name:   e.Name(),
					Size:   info.Size(),
					Mode:   info.Mode(),
					Type:   entryType,
					Target: target,
				})
			}
			if offset >= len(filtered) {
				return ListResult{}, nil
			}
			filtered = filtered[offset:]
			if limit > 0 && limit < len(filtered) {
				next := offset + limit
				return ListResult{
					Entries:    filtered[:limit],
					NextOffset: &next,
					Remaining:  len(filtered) - limit,
				}, nil
			}
			return ListResult{Entries: filtered}, nil
		},

		SearchCodebase: func(ctx context.Context, path, pattern, filter string, offset, limit int) (SearchResult, error) {
			if err := checkOffset(int64(offset)); err != nil {
				return SearchResult{}, err
			}
			if offset > maxSearchOffset {
				return SearchResult{}, fmt.Errorf("search offset exceeds %d; narrow the path or filter", maxSearchOffset)
			}
			if limit <= 0 || limit > maxSearchMatches {
				limit = maxSearchMatches
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return SearchResult{}, fmt.Errorf("invalid pattern: %w", err)
			}
			var result SearchResult
			skip := offset
			if err := localWalkDir(ctx, r, path, filter, func(relPath string, data []byte) error {
				// One extra match distinguishes a full final page from one
				// with a continuation, without collecting the rest of the tree.
				for _, loc := range re.FindAllIndex(data, skip+limit-len(result.Matches)+1) {
					if skip > 0 {
						skip--
						continue
					}
					if len(result.Matches) == limit {
						next := offset + len(result.Matches)
						if next > maxSearchOffset {
							return fmt.Errorf("search continuation exceeds offset %d; narrow the path or filter", maxSearchOffset)
						}
						result.NextOffset = &next
						result.HasMore = true
						return fs.SkipAll
					}
					result.Matches = append(result.Matches, Match{
						Path:   relPath,
						Offset: int64(loc[0]),
						Length: loc[1] - loc[0],
					})
				}
				return nil
			}); err != nil {
				return SearchResult{}, err
			}
			return result, nil
		},
	}
}

// indentationNote renders the EditResult.Note for an edit that matched after
// an indentation shift; it is empty for the zero shift.
func indentationNote(shift textedit.Shift) string {
	if shift.Whitespace == "" {
		return ""
	}
	return fmt.Sprintf("old_string matched after adjusting indentation (%s); new_string was shifted the same way", shift)
}

// checkOffset rejects a negative pagination offset. Offsets reach these
// callbacks from model-generated tool calls and feed slice expressions below, so
// a negative value has to surface as an error rather than a panic.
func checkOffset(offset int64) error {
	if offset < 0 {
		return fmt.Errorf("offset must be >= 0, got %d", offset)
	}
	return nil
}

// localWalkDir recursively walks the directory tree under r starting at dir
// via fs.WalkDir, calling fn for each regular file that matches the optional
// filter glob. Hidden directories (names starting with ".") are skipped.
//
// dir is cleaned first, because fs.ValidPath rejects equivalent forms such as
// "./os", "os/" and "a/../os". An error on the walk root is returned wrapped.
// Unreadable entries below the root are skipped. Oversized files fail the
// search so incomplete coverage is not reported as a complete result.
func localWalkDir(ctx context.Context, r *os.Root, dir, filter string, fn func(relPath string, data []byte) error) error {
	dir = path.Clean(dir)
	return fs.WalkDir(r.FS(), dir, func(name string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			if name == dir { // d may be nil here
				return fmt.Errorf("search root %q: %w", dir, err)
			}
			return nil
		}
		if d.IsDir() {
			if name != dir && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if filter != "" {
			matched, matchErr := filepath.Match(filter, d.Name())
			if matchErr != nil || !matched {
				return nil
			}
		}
		data, readErr := readSearchFile(r, name)
		if readErr != nil {
			if errors.Is(readErr, errSearchFileTooLarge) {
				return readErr
			}
			return nil
		}
		return fn(name, data)
	})
}

func readSearchFile(r *os.Root, name string) ([]byte, error) {
	f, err := openRegularFile(r, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSearchFileBytes {
		return nil, fmt.Errorf("%s: %w (%d bytes); narrow the path or filter", name, errSearchFileTooLarge, maxSearchFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSearchFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSearchFileBytes {
		return nil, fmt.Errorf("%s: %w (%d bytes); narrow the path or filter", name, errSearchFileTooLarge, maxSearchFileBytes)
	}
	return data, nil
}

func openRegularFile(r *os.Root, name string) (*os.File, error) {
	// Check before opening so a staged FIFO does not block waiting for a writer.
	info, err := r.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return f, nil
}
