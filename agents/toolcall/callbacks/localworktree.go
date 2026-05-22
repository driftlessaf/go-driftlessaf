/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LocalWorktree returns a WorktreeCallbacks backed by r, an os.Root that
// sandboxes all file operations within a single directory tree. Path traversal
// outside the root is prevented by the os.Root implementation.
func LocalWorktree(r *os.Root) WorktreeCallbacks {
	return WorktreeCallbacks{
		ReadFile: func(_ context.Context, path string, offset int64, limit int) (ReadResult, error) {
			data, err := r.ReadFile(path)
			if err != nil {
				return ReadResult{}, err
			}
			if offset > int64(len(data)) {
				return ReadResult{}, nil
			}
			data = data[offset:]
			if limit < 0 || int64(limit) >= int64(len(data)) {
				return ReadResult{Content: string(data)}, nil
			}
			next := offset + int64(limit)
			remaining := int64(len(data)) - int64(limit)
			return ReadResult{
				Content:    string(data[:limit]),
				NextOffset: &next,
				Remaining:  remaining,
			}, nil
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
			if count == 0 {
				return EditResult{}, fmt.Errorf("string not found in %q", path)
			}
			if !replaceAll && count > 1 {
				return EditResult{}, fmt.Errorf("string found %d times in %q; use replace_all=true to replace all occurrences", count, path)
			}
			var updated string
			if replaceAll {
				updated = strings.ReplaceAll(content, oldString, newString)
			} else {
				updated = strings.Replace(content, oldString, newString, 1)
			}
			info, err := r.Stat(path)
			if err != nil {
				return EditResult{}, err
			}
			if err := r.WriteFile(path, []byte(updated), info.Mode()); err != nil {
				return EditResult{}, err
			}
			replacements := 1
			if replaceAll {
				replacements = count
			}
			return EditResult{Replacements: replacements}, nil
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

		SearchCodebase: func(_ context.Context, path, pattern, filter string, offset, limit int) (SearchResult, error) {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return SearchResult{}, fmt.Errorf("invalid pattern: %w", err)
			}
			var allMatches []Match
			if err := localWalkDir(r, path, filter, func(relPath string, data []byte) {
				for _, loc := range re.FindAllIndex(data, -1) {
					allMatches = append(allMatches, Match{
						Path:   relPath,
						Offset: int64(loc[0]),
						Length: loc[1] - loc[0],
					})
				}
			}); err != nil {
				return SearchResult{}, err
			}
			if offset >= len(allMatches) {
				return SearchResult{}, nil
			}
			allMatches = allMatches[offset:]
			if limit > 0 && limit < len(allMatches) {
				next := offset + limit
				return SearchResult{
					Matches:    allMatches[:limit],
					NextOffset: &next,
					HasMore:    true,
				}, nil
			}
			return SearchResult{Matches: allMatches}, nil
		},
	}
}

// localWalkDir recursively walks the directory tree under r starting at dir
// via fs.WalkDir, calling fn for each regular file that matches the optional
// filter glob. Hidden directories (names starting with ".") are skipped.
func localWalkDir(r *os.Root, dir, filter string, fn func(relPath string, data []byte)) error {
	return fs.WalkDir(r.FS(), dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
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
		data, readErr := r.ReadFile(path)
		if readErr != nil {
			return nil
		}
		fn(path, data)
		return nil
	})
}
