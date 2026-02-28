/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"github.com/waigani/diffparser"
)

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
		// Line 0 means the diagnostic applies to the whole file;
		// include it if the file has any changes.
		if d.Line == 0 {
			filtered = append(filtered, d)
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
