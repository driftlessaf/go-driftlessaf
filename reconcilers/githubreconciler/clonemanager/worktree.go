/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	gogit "github.com/go-git/go-git/v5"
)

// WorktreeCallbacks creates callbacks.WorktreeCallbacks bound to a git worktree.
// All file operations are scoped to the worktree root directory.
// Write and delete operations automatically stage changes to the git index.
func WorktreeCallbacks(wt *gogit.Worktree) callbacks.WorktreeCallbacks {
	root := wt.Filesystem.Root()

	// validatePath ensures path doesn't escape the worktree root
	validatePath := func(path string) (string, error) {
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

	return callbacks.WorktreeCallbacks{
		ReadFile: func(_ context.Context, path string) (string, error) {
			fullPath, err := validatePath(path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
		WriteFile: func(_ context.Context, path, content string, mode os.FileMode) error {
			fullPath, err := validatePath(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(fullPath, []byte(content), mode); err != nil {
				return err
			}
			// Auto-stage the written file
			_, err = wt.Add(path)
			return err
		},
		DeleteFile: func(_ context.Context, path string) error {
			fullPath, err := validatePath(path)
			if err != nil {
				return err
			}
			if err := os.Remove(fullPath); err != nil {
				return err
			}
			// Auto-stage the deletion
			_, err = wt.Remove(path)
			return err
		},
		ListDirectory: func(_ context.Context, path string) ([]string, error) {
			fullPath, err := validatePath(path)
			if err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(fullPath)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			return names, nil
		},
		SearchCodebase: func(_ context.Context, pattern string) ([]callbacks.Match, error) {
			return grepWorktree(root, pattern)
		},
	}
}

// grepWorktree searches for a pattern in all files under root.
func grepWorktree(root, pattern string) ([]callbacks.Match, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	var matches []callbacks.Match
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Skip files we can't access
		}

		// Skip directories and hidden files/folders
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip binary files by checking extension
		if isBinaryFile(path) {
			return nil
		}

		// Search file contents
		fileMatches, err := searchFile(path, root, re)
		if err != nil {
			return nil // Skip files we can't read
		}
		matches = append(matches, fileMatches...)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return matches, nil
}

func searchFile(path, root string, re *regexp.Regexp) ([]callbacks.Match, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	relPath, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}

	var matches []callbacks.Match
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) {
			matches = append(matches, callbacks.Match{
				Path:    relPath,
				Line:    lineNum,
				Content: line,
			})
		}
	}

	return matches, scanner.Err()
}

func isBinaryFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	binaryExts := map[string]struct{}{
		".exe": {}, ".dll": {}, ".so": {}, ".dylib": {},
		".zip": {}, ".tar": {}, ".gz": {}, ".bz2": {},
		".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".ico": {},
		".pdf": {}, ".doc": {}, ".docx": {},
		".bin": {}, ".dat": {},
	}
	_, isBinary := binaryExts[ext]
	return isBinary
}
