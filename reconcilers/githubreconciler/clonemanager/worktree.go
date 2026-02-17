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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	gogit "github.com/go-git/go-git/v5"
)

// WorktreeCallbacks creates callbacks.WorktreeCallbacks bound to a git worktree.
// All file operations are scoped to the worktree root directory.
// Write, delete, move, copy, symlink, and chmod operations automatically stage
// changes to the git index.
func WorktreeCallbacks(wt *gogit.Worktree) callbacks.WorktreeCallbacks {
	root := wt.Filesystem.Root()

	return callbacks.WorktreeCallbacks{
		ReadFile: func(_ context.Context, path string, offset int64, limit int) (callbacks.ReadResult, error) {
			fullPath, err := validatePath(root, path)
			if err != nil {
				return callbacks.ReadResult{}, err
			}

			if isBinaryFile(fullPath) {
				return callbacks.ReadResult{}, fmt.Errorf("file %q appears to be binary", path)
			}

			f, err := os.Open(fullPath)
			if err != nil {
				return callbacks.ReadResult{}, err
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

			// Determine how many bytes to read.
			readSize := int64(limit)
			if limit < 0 {
				readSize = fileSize - offset
			}
			if remaining := fileSize - offset; readSize > remaining {
				readSize = remaining
			}

			if offset > 0 {
				if _, err := f.Seek(offset, io.SeekStart); err != nil {
					return callbacks.ReadResult{}, err
				}
			}

			buf := make([]byte, readSize)
			n, err := io.ReadFull(f, buf)
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return callbacks.ReadResult{}, err
			}
			buf = buf[:n]

			// If we truncated mid-file, adjust to avoid splitting a UTF-8 character.
			bytesAfterRead := fileSize - offset - int64(n)
			if bytesAfterRead > 0 {
				buf = adjustUTF8Boundary(buf)
			}

			actualRead := int64(len(buf))
			afterRead := offset + actualRead
			bytesRemaining := fileSize - afterRead

			var result callbacks.ReadResult
			result.Content = string(buf)
			result.Remaining = bytesRemaining
			if bytesRemaining > 0 {
				result.NextOffset = &afterRead
			}
			return result, nil
		},

		WriteFile: func(_ context.Context, path, content string, mode os.FileMode) error {
			fullPath, err := validatePath(root, path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(fullPath, []byte(content), mode); err != nil {
				return err
			}
			_, err = wt.Add(path)
			return err
		},

		DeleteFile: func(_ context.Context, path string) error {
			fullPath, err := validatePath(root, path)
			if err != nil {
				return err
			}
			if err := os.Remove(fullPath); err != nil {
				return err
			}
			_, err = wt.Remove(path)
			return err
		},

		MoveFile: func(_ context.Context, src, dst string) error {
			srcFull, err := validatePath(root, src)
			if err != nil {
				return err
			}
			dstFull, err := validatePath(root, dst)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dstFull), 0755); err != nil {
				return err
			}
			if err := os.Rename(srcFull, dstFull); err != nil {
				return err
			}
			if _, err := wt.Remove(src); err != nil {
				return err
			}
			_, err = wt.Add(dst)
			return err
		},

		CopyFile: func(_ context.Context, src, dst string) error {
			srcFull, err := validatePath(root, src)
			if err != nil {
				return err
			}
			dstFull, err := validatePath(root, dst)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(srcFull)
			if err != nil {
				return err
			}
			srcInfo, err := os.Stat(srcFull)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dstFull), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dstFull, data, srcInfo.Mode()); err != nil {
				return err
			}
			_, err = wt.Add(dst)
			return err
		},

		CreateSymlink: func(_ context.Context, path, target string) error {
			fullPath, err := validatePath(root, path)
			if err != nil {
				return err
			}
			if err := validateSymlinkTarget(root, fullPath, target); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				return err
			}
			if err := os.Symlink(target, fullPath); err != nil {
				return err
			}
			_, err = wt.Add(path)
			return err
		},

		Chmod: func(_ context.Context, path string, mode os.FileMode) error {
			fullPath, err := validatePath(root, path)
			if err != nil {
				return err
			}
			if err := os.Chmod(fullPath, mode); err != nil {
				return err
			}
			_, err = wt.Add(path)
			return err
		},

		ListDirectory: func(_ context.Context, path, filter string, offset, limit int) (callbacks.ListResult, error) {
			fullPath, err := validatePath(root, path)
			if err != nil {
				return callbacks.ListResult{}, err
			}

			entries, err := os.ReadDir(fullPath)
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
				de, err := buildDirEntry(fullPath, e)
				if err != nil {
					continue // Skip entries we can't stat.
				}
				result.Entries = append(result.Entries, de)
			}

			return result, nil
		},

		EditFile: func(_ context.Context, path, oldString, newString string, replaceAll bool) (callbacks.EditResult, error) {
			if len(oldString) == 0 {
				return callbacks.EditResult{}, errors.New("old_string must not be empty")
			}
			if len(oldString) > maxEditStringSize {
				return callbacks.EditResult{}, fmt.Errorf("old_string is %d bytes; use write_file for large replacements", len(oldString))
			}
			if len(newString) > maxEditStringSize {
				return callbacks.EditResult{}, fmt.Errorf("new_string is %d bytes; use write_file for large replacements", len(newString))
			}

			fullPath, err := validatePath(root, path)
			if err != nil {
				return callbacks.EditResult{}, err
			}

			// Plan: stream the file to find match offsets.
			offsets, err := planReplacements(fullPath, []byte(oldString), replaceAll)
			if err != nil {
				return callbacks.EditResult{}, err
			}
			if len(offsets) == 0 {
				return callbacks.EditResult{}, errors.New("old_string not found in file")
			}

			// Execute: stream the file again, replacing at recorded offsets.
			if err := executeReplacements(fullPath, offsets, len(oldString), []byte(newString)); err != nil {
				return callbacks.EditResult{}, err
			}

			if _, err := wt.Add(path); err != nil {
				return callbacks.EditResult{}, err
			}
			return callbacks.EditResult{Replacements: len(offsets)}, nil
		},

		SearchCodebase: func(_ context.Context, searchPath, pattern, filter string, offset, limit int) (callbacks.SearchResult, error) {
			searchRoot, err := validatePath(root, searchPath)
			if err != nil {
				return callbacks.SearchResult{}, err
			}

			re, err := regexp.Compile(pattern)
			if err != nil {
				return callbacks.SearchResult{}, fmt.Errorf("invalid pattern: %w", err)
			}

			// Collect offset+limit+1 matches in a single pass so we can
			// stop walking as soon as we know the page is full.
			need := offset + limit + 1 // +1 to detect whether more remain
			var allMatches []callbacks.Match

			err = filepath.WalkDir(searchRoot, func(filePath string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil // Skip inaccessible entries.
				}

				if d.IsDir() {
					if strings.HasPrefix(d.Name(), ".") {
						return filepath.SkipDir
					}
					return nil
				}

				if isBinaryFile(filePath) {
					return nil
				}

				if !matchFilter(d.Name(), filter) {
					return nil
				}

				// Paths are always relative to the worktree root.
				relPath, err := filepath.Rel(root, filePath)
				if err != nil {
					return nil
				}

				data, err := os.ReadFile(filePath)
				if err != nil {
					return nil // Skip unreadable files.
				}

				for _, loc := range re.FindAllIndex(data, -1) {
					allMatches = append(allMatches, callbacks.Match{
						Path:   filepath.ToSlash(relPath),
						Offset: int64(loc[0]),
						Length: loc[1] - loc[0],
					})
					if len(allMatches) >= need {
						return filepath.SkipAll
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

// validatePath ensures path doesn't escape the worktree root via ".." traversal.
func validatePath(root, path string) (string, error) {
	fullPath := filepath.Join(root, filepath.Clean(path))
	rel, err := filepath.Rel(root, fullPath)
	if err != nil {
		return "", fmt.Errorf("path %q: %w", path, err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes worktree", path)
	}
	return fullPath, nil
}

// validateSymlinkTarget checks that a symlink target will not escape the
// worktree root. Absolute targets are always rejected. Relative targets are
// resolved from the symlink's parent directory to verify they stay within root.
func validateSymlinkTarget(root, linkFullPath, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("symlink target %q is absolute; only relative targets are allowed", target)
	}

	// Resolve the target relative to the symlink's parent directory.
	linkDir := filepath.Dir(linkFullPath)
	effectivePath := filepath.Clean(filepath.Join(linkDir, target))

	rel, err := filepath.Rel(root, effectivePath)
	if err != nil {
		return fmt.Errorf("symlink target %q resolves outside worktree", target)
	}
	if strings.HasPrefix(rel, "..") {
		return fmt.Errorf("symlink target %q resolves outside worktree", target)
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

// buildDirEntry creates a callbacks.DirEntry from an os.DirEntry.
func buildDirEntry(parentPath string, e os.DirEntry) (callbacks.DirEntry, error) {
	fullPath := filepath.Join(parentPath, e.Name())

	// Use Lstat so symlinks are not followed.
	fi, err := os.Lstat(fullPath)
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
		if target, err := os.Readlink(fullPath); err == nil {
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
func planReplacements(path string, pattern []byte, replaceAll bool) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

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
func executeReplacements(path string, offsets []int64, oldLen int, newBytes []byte) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	fi, err := src.Stat()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".edit-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
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

	return os.Rename(tmp.Name(), path)
}
