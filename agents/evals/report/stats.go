/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/evals"
)

// stats aggregates the pass/fail and grade metrics shared by every report
// level (evaluation, model, and test case). The count fields are named
// distinctly from the raw failures/grades slices held by testCaseResult so
// embedding cannot shadow them.
type stats struct {
	failureCount int64
	gradeCount   int64
	gradeScore   float64
	iterations   int64
	passRate     float64
	avgGrade     float64
}

// add accumulates raw collector results into the aggregate counts.
func (s *stats) add(failures []string, grades []evals.Grade, iterations int64) {
	s.failureCount += int64(len(failures))
	s.gradeCount += int64(len(grades))
	for _, grade := range grades {
		s.gradeScore += grade.Score
	}
	s.iterations += iterations
}

// finalize computes the pass rate and average grade from the accumulated counts.
func (s *stats) finalize() {
	if s.iterations > 0 {
		s.passRate = float64(s.iterations-s.failureCount) / float64(s.iterations)
	}
	if s.gradeCount > 0 {
		s.avgGrade = s.gradeScore / float64(s.gradeCount)
	}
}

// formatForTree formats the metrics as a value and label for tree display.
func (s *stats) formatForTree() (string, string) {
	hasPassRate := s.failureCount > 0
	hasGrades := s.gradeCount > 0

	switch {
	case hasPassRate && hasGrades:
		value := fmt.Sprintf("%.1f%% pass, %.2f avg", s.passRate*100, s.avgGrade)
		label := fmt.Sprintf("(%d/%d)", s.iterations-s.failureCount, s.iterations)
		return value, label
	case hasGrades:
		gradeWord := "results"
		if s.gradeCount == 1 {
			gradeWord = "result"
		}
		value := fmt.Sprintf("%.2f avg", s.avgGrade)
		label := fmt.Sprintf("(%d %s)", s.gradeCount, gradeWord)
		return value, label
	default:
		value := fmt.Sprintf("%.1f%%", s.passRate*100)
		label := fmt.Sprintf("(%d/%d)", s.iterations-s.failureCount, s.iterations)
		return value, label
	}
}

// belowThreshold reports whether the pass rate or average grade falls below the threshold.
func (s *stats) belowThreshold(threshold float64) bool {
	if s.passRate < threshold {
		return true
	}
	return s.gradeCount > 0 && s.avgGrade < threshold
}
