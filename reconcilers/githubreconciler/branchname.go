/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"strings"
)

// PathToBranchSuffix sanitizes a file path for use as a git branch name suffix.
// Git rejects ref names where a path component starts with "." (a "funny refname"),
// so this function replaces leading dots with "_dot_" in each path component.
func PathToBranchSuffix(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ".") {
			parts[i] = "_dot_" + part[1:]
		}
	}
	return strings.Join(parts, "/")
}

// BranchSuffixToPath reverses PathToBranchSuffix, converting a sanitized branch
// name suffix back to the original file path.
func BranchSuffixToPath(suffix string) string {
	parts := strings.Split(suffix, "/")
	for i, part := range parts {
		if rest, ok := strings.CutPrefix(part, "_dot_"); ok {
			parts[i] = "." + rest
		}
	}
	return strings.Join(parts, "/")
}
