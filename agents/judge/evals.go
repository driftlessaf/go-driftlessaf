/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/evals"
)

// ValidScore returns an ObservableTraceCallback that validates the judgment score is in the correct range for the mode
func ValidScore(mode JudgmentMode) evals.ObservableTraceCallback[*Judgement] {
	return evals.ResultValidator(func(result *Judgement) error {
		switch mode {
		case GoldenMode:
			if result.Score < 0 || result.Score > 1 {
				return fmt.Errorf("score %.2f is out of range [0, 1] for golden mode", result.Score)
			}
		case BenchmarkMode:
			if result.Score < -1 || result.Score > 1 {
				return fmt.Errorf("score %.2f is out of range [-1, 1] for benchmark mode", result.Score)
			}
		case StandaloneMode:
			if result.Score < 0 || result.Score > 1 {
				return fmt.Errorf("score %.2f is out of range [0, 1] for standalone mode", result.Score)
			}
		default:
			return fmt.Errorf("unknown judgment mode: %s", mode)
		}
		return nil
	})
}

// HasReasoning returns an ObservableTraceCallback that validates the judgment includes reasoning
func HasReasoning() evals.ObservableTraceCallback[*Judgement] {
	return evals.ResultValidator(func(result *Judgement) error {
		if result.Reasoning == "" {
			return errors.New("judgment has no reasoning")
		}
		return nil
	})
}

// CheckMode returns an ObservableTraceCallback that checks the judgment mode matches the expected value
func CheckMode(expectedMode JudgmentMode) evals.ObservableTraceCallback[*Judgement] {
	return evals.ResultValidator(func(result *Judgement) error {
		if result.Mode != expectedMode {
			return fmt.Errorf("mode %s does not match expected %s", result.Mode, expectedMode)
		}
		return nil
	})
}

// ScoreRange returns an ObservableTraceCallback that grades the judgment score based on how well it fits the expected range
func ScoreRange(minScore, maxScore float64) evals.ObservableTraceCallback[*Judgement] {
	return func(o evals.Observer, trace *evals.Trace[*Judgement]) {
		if trace.Result == nil {
			o.Fail("judgment result is nil")
			return
		}

		// Calculate grade based on how well score fits expected range
		grade := calculateRangeGrade(trace.Result.Score, minScore, maxScore)

		// Report the grade with reasoning about range fit
		var reasoning string
		if grade == 1.0 {
			reasoning = fmt.Sprintf("score %.2f is within expected range [%.2f, %.2f]", trace.Result.Score, minScore, maxScore)
		} else {
			reasoning = fmt.Sprintf("score %.2f is outside expected range [%.2f, %.2f]", trace.Result.Score, minScore, maxScore)
		}

		o.Grade(grade, reasoning)
	}
}

// calculateRangeGrade computes a grade from 0.0 to 1.0 based on how well a score fits an expected range
func calculateRangeGrade(actualScore, minScore, maxScore float64) float64 {
	// Perfect score for within range
	if actualScore >= minScore && actualScore <= maxScore {
		return 1.0
	}

	// Calculate distance from closest boundary
	distanceFromMin := abs(actualScore - minScore)
	distanceFromMax := abs(actualScore - maxScore)

	distance := distanceFromMin
	if distanceFromMax < distanceFromMin {
		distance = distanceFromMax
	}

	// Calculate penalty based on range width
	rangeWidth := maxScore - minScore
	penalty := distance / (rangeWidth * 2)
	if penalty > 1.0 {
		penalty = 1.0
	}

	grade := 1.0 - penalty
	if grade < 0.0 {
		grade = 0.0
	}

	return grade
}

// abs returns the absolute value of a float64
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
