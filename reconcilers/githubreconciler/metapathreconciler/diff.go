/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/waigani/diffparser"
)

type reviewDiffKey struct{}

// filterDiffToPaths retains complete Git file patches for selected destination
// paths. Parse each patch to identify its destination, but preserve its bytes:
// rebuilding hunks would risk changing line numbers or losing newline markers.
// Git prefixes hunk content with a space, +, or -, so a file's content cannot
// be mistaken for the next unprefixed "diff --git" header.
func filterDiffToPaths(raw string, paths []string) (string, error) {
	var filtered strings.Builder
	for raw != "" {
		patch, rest, more := strings.Cut(raw, "\ndiff --git ")
		if more {
			patch += "\n"
			raw = "diff --git " + rest
		} else {
			raw = ""
		}
		parsed, err := diffparser.Parse(patch)
		if err != nil {
			return "", fmt.Errorf("parse review file diff: %w", err)
		}
		if len(parsed.Files) != 1 {
			return "", fmt.Errorf("expected one file in review diff section, got %d", len(parsed.Files))
		}
		file := parsed.Files[0]
		if file.NewName != "" && file.NewName != "/dev/null" && slices.Contains(paths, file.NewName) {
			filtered.WriteString(patch)
		}
	}
	return filtered.String(), nil
}

// WithReviewDiff marks an analyzer invocation as a PR review and carries the
// unified diff from the PR base to its head. Path audits do not set this value.
// An empty diff still identifies a PR review; it must not trigger a broad audit.
func WithReviewDiff(ctx context.Context, diff string) context.Context {
	return context.WithValue(ctx, reviewDiffKey{}, diff)
}

// ReviewDiffFromContext returns the PR diff and whether this is a PR review.
// Analyzers can read surrounding code for context, but should report only
// violations introduced by the diff, anchored to an added or modified line.
func ReviewDiffFromContext(ctx context.Context) (string, bool) {
	diff, ok := ctx.Value(reviewDiffKey{}).(string)
	return diff, ok
}

// changedLineRange represents a range of changed lines in a file.
type changedLineRange struct {
	start, end int
}

// parsedDiff holds the results of parsing a unified diff: the set of changed
// file paths and, for each file, the line ranges that were modified.
type parsedDiff struct {
	// files is the list of changed file paths (new names, excluding deletions).
	files []string
	// ranges maps each file path to its changed line ranges.
	ranges map[string][]*changedLineRange
}

// parseDiff parses a unified diff string and returns the changed files and
// their modified line ranges. Deleted files (where NewName is /dev/null) are
// excluded since there is nothing to analyze.
func parseDiff(rawDiff string) (*parsedDiff, error) {
	diff, err := diffparser.Parse(rawDiff)
	if err != nil {
		return nil, err
	}

	result := &parsedDiff{
		ranges: make(map[string][]*changedLineRange, len(diff.Files)),
	}
	for _, f := range diff.Files {
		// Skip deletions.
		if f.NewName == "/dev/null" || f.NewName == "" {
			continue
		}
		result.files = append(result.files, f.NewName)
		for _, h := range f.Hunks {
			var cur *changedLineRange
			for _, l := range h.NewRange.Lines {
				if l.Mode != diffparser.ADDED {
					cur = nil
					continue
				}
				if cur != nil && l.Number == cur.end+1 {
					cur.end = l.Number
				} else {
					cur = &changedLineRange{start: l.Number, end: l.Number}
					result.ranges[f.NewName] = append(result.ranges[f.NewName], cur)
				}
			}
		}
	}
	return result, nil
}

// filterToChangedLines filters diagnostics to only those on lines that were
// changed in the diff. This ensures annotations only appear on lines the PR
// author actually touched.
func filterToChangedLines(diagnostics []Diagnostic, pd *parsedDiff) []Diagnostic {
	filtered := make([]Diagnostic, 0, len(diagnostics))
	for _, d := range diagnostics {
		ranges, ok := pd.ranges[d.Path]
		if !ok {
			continue
		}
		// File-level findings cannot establish that the PR introduced the
		// issue. Require an actual changed line, including for new files.
		if d.Line <= 0 {
			continue
		}
		for _, r := range ranges {
			if d.Line >= r.start && d.Line <= r.end {
				filtered = append(filtered, d)
				break
			}
		}
	}
	return filtered
}
