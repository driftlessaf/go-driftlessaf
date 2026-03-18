/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/go-github/v84/github"
)

// CheckDetails holds the diagnostics found during PR analysis.
// It implements the statusmanager.Annotated and Markdown() interfaces
// so that diagnostics appear as check run annotations and markdown output.
type CheckDetails struct {
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	Identity    string       `json:"-"`
}

// maxAnnotations is the GitHub API limit for check run annotations per update.
const maxAnnotations = 50

// Annotations converts diagnostics to GitHub check run annotations.
func (d CheckDetails) Annotations() []*github.CheckRunAnnotation {
	limit := min(len(d.Diagnostics), maxAnnotations)
	annotations := make([]*github.CheckRunAnnotation, 0, limit)
	for _, diag := range d.Diagnostics[:limit] {
		line := diag.Line
		if line == 0 {
			line = 1
		}
		annotations = append(annotations, &github.CheckRunAnnotation{
			Path:            github.Ptr(diag.Path),
			StartLine:       github.Ptr(line),
			EndLine:         github.Ptr(line),
			AnnotationLevel: github.Ptr("warning"),
			Title:           github.Ptr(diag.Rule),
			Message:         github.Ptr(diag.Message),
		})
	}
	return annotations
}

// Markdown renders the diagnostics as a markdown summary for the check run.
func (d CheckDetails) Markdown() string {
	if len(d.Diagnostics) == 0 {
		return ""
	}

	var sb strings.Builder

	// Collect unique files.
	seen := make(map[string]struct{}, len(d.Diagnostics))
	for _, diag := range d.Diagnostics {
		seen[diag.Path] = struct{}{}
	}
	sb.WriteString("**Files with issues:**\n")
	for path := range seen {
		fmt.Fprintf(&sb, "- `%s`\n", path)
	}

	sb.WriteString("\n| File | Line | Rule | Message |\n")
	sb.WriteString("|------|------|------|---------|\n")
	for _, diag := range d.Diagnostics {
		line := "-"
		if diag.Line > 0 {
			line = strconv.Itoa(diag.Line)
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s |\n", diag.Path, line, diag.Rule, diag.Message)
	}

	if d.Identity != "" {
		fmt.Fprintf(&sb, "\nTo skip this check, apply the `skip:%s` label to the PR.\n", d.Identity)
	}

	return sb.String()
}
