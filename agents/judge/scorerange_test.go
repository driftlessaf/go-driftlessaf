/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
)

// mockObserver implements the evals.Observer interface for testing
type mockObserver struct {
	grades     []gradeCall
	failures   []string
	logs       []string
	totalCount int64
}

type gradeCall struct {
	grade     float64
	reasoning string
}

func (m *mockObserver) Grade(grade float64, reasoning string) {
	m.grades = append(m.grades, gradeCall{grade: grade, reasoning: reasoning})
}

func (m *mockObserver) Fail(reason string) {
	m.failures = append(m.failures, reason)
}

func (m *mockObserver) Log(message string) {
	m.logs = append(m.logs, message)
}

func (m *mockObserver) Increment() {
	m.totalCount++
}

func (m *mockObserver) Total() int64 {
	return m.totalCount
}

func TestScoreRange_ComprehensiveNestedLoop(t *testing.T) {
	const increment = 0.01

	for min := -1.0; min <= 1.0; min += increment {
		min = roundToIncrement(min, increment) // Handle floating point precision
		for max := min; max <= 1.0; max += increment {
			max = roundToIncrement(max, increment) // Handle floating point precision
			for value := -1.0; value <= 1.0; value += increment {
				value = roundToIncrement(value, increment) // Handle floating point precision

				// Create mock observer
				observer := &mockObserver{}

				// Call ScoreRange function
				ScoreRange(min, max)(observer, &evals.Trace[*Judgement]{
					Result: &Judgement{
						Score: value,
					},
				})

				// Verify no failures occurred
				if len(observer.failures) > 0 {
					t.Errorf("min=%.2f,max=%.2f,value=%.2f: unexpected failures: got = %v, wanted = []", min, max, value, observer.failures)
					continue
				}

				// Verify exactly one grade was recorded
				if len(observer.grades) != 1 {
					t.Errorf("min=%.2f,max=%.2f,value=%.2f: grade calls: got = %d, wanted = 1", min, max, value, len(observer.grades))
					continue
				}

				grade := observer.grades[0].grade

				// Verify grade is within valid bounds [0, 1]
				if grade < 0.0 || grade > 1.0 {
					t.Errorf("min=%.2f,max=%.2f,value=%.2f: grade out of bounds: got = %f, wanted = [0.0, 1.0]", min, max, value, grade)
				}

				// Verify grade is 1.0 when value is within [min, max]
				if value >= min && value <= max {
					if grade != 1.0 {
						t.Errorf("min=%.2f,max=%.2f,value=%.2f: grade for value in range: got = %f, wanted = 1.0", min, max, value, grade)
					}
				} else {
					// Verify grade is less than 1.0 when value is outside [min, max]
					if grade >= 1.0 {
						t.Errorf("min=%.2f,max=%.2f,value=%.2f: grade for value outside range: got = %f, wanted = <1.0", min, max, value, grade)
					}
				}

				// Verify reasoning is provided
				if observer.grades[0].reasoning == "" {
					t.Errorf("min=%.2f,max=%.2f,value=%.2f: reasoning: got = empty, wanted = non-empty", min, max, value)
				}
			}
		}
	}
}

func TestScoreRange_CalculateRangeGrade(t *testing.T) {
	tests := []struct {
		name        string
		actualScore float64
		minScore    float64
		maxScore    float64
		wantGrade   float64
	}{{
		name:        "value_at_min_boundary",
		actualScore: 0.5,
		minScore:    0.5,
		maxScore:    0.8,
		wantGrade:   1.0,
	}, {
		name:        "value_at_max_boundary",
		actualScore: 0.8,
		minScore:    0.5,
		maxScore:    0.8,
		wantGrade:   1.0,
	}, {
		name:        "value_in_middle",
		actualScore: 0.65,
		minScore:    0.5,
		maxScore:    0.8,
		wantGrade:   1.0,
	}, {
		name:        "value_below_range",
		actualScore: 0.3,
		minScore:    0.5,
		maxScore:    0.8,
		wantGrade:   0.666667, // 1 - (0.2 / (0.3 * 2)) = 1 - (0.2 / 0.6) = 1 - 0.333 = 0.667
	}, {
		name:        "value_above_range",
		actualScore: 1.0,
		minScore:    0.5,
		maxScore:    0.8,
		wantGrade:   0.666667, // 1 - (0.2 / (0.3 * 2)) = 1 - (0.2 / 0.6) = 1 - 0.333 = 0.667
	},
		// Extreme cases - perfect score ranges
		{
			name:        "perfect_score_range_exact_match",
			actualScore: 1.0,
			minScore:    1.0,
			maxScore:    1.0,
			wantGrade:   1.0,
		}, {
			name:        "perfect_score_range_below",
			actualScore: 0.99,
			minScore:    1.0,
			maxScore:    1.0,
			wantGrade:   0.0, // 1 - (0.01 / 0) = 0 due to zero range width protection
		}, {
			name:        "perfect_score_range_above",
			actualScore: 1.01,
			minScore:    1.0,
			maxScore:    1.0,
			wantGrade:   0.0, // 1 - (0.01 / 0) = 0 due to zero range width protection
		},
		// Zero-based ranges
		{
			name:        "zero_min_range_at_zero",
			actualScore: 0.0,
			minScore:    0.0,
			maxScore:    0.0,
			wantGrade:   1.0,
		}, {
			name:        "zero_min_range_slightly_above",
			actualScore: 0.01,
			minScore:    0.0,
			maxScore:    0.0,
			wantGrade:   0.0, // Zero range width
		},
		// Very narrow ranges (realistic from integration tests)
		{
			name:        "narrow_range_0.0_to_0.1_inside",
			actualScore: 0.05,
			minScore:    0.0,
			maxScore:    0.1,
			wantGrade:   1.0,
		}, {
			name:        "narrow_range_0.0_to_0.1_outside",
			actualScore: 0.2,
			minScore:    0.0,
			maxScore:    0.1,
			wantGrade:   0.5, // 1 - (0.1 / (0.1 * 2)) = 1 - (0.1 / 0.2) = 0.5
		}, {
			name:        "narrow_range_0.0_to_0.2_inside",
			actualScore: 0.1,
			minScore:    0.0,
			maxScore:    0.2,
			wantGrade:   1.0,
		}, {
			name:        "narrow_range_0.0_to_0.2_outside",
			actualScore: 0.4,
			minScore:    0.0,
			maxScore:    0.2,
			wantGrade:   0.5, // 1 - (0.2 / (0.2 * 2)) = 1 - (0.2 / 0.4) = 0.5
		}, {
			name:        "narrow_range_0.0_to_0.3_inside",
			actualScore: 0.15,
			minScore:    0.0,
			maxScore:    0.3,
			wantGrade:   1.0,
		}, {
			name:        "narrow_range_0.0_to_0.3_far_outside",
			actualScore: 0.9,
			minScore:    0.0,
			maxScore:    0.3,
			wantGrade:   0.0, // 1 - (0.6 / (0.3 * 2)) = 1 - (0.6 / 0.6) = 0
		},
		// Mid-range quality ranges (from integration tests)
		{
			name:        "mid_quality_0.2_to_0.4_inside",
			actualScore: 0.3,
			minScore:    0.2,
			maxScore:    0.4,
			wantGrade:   1.0,
		}, {
			name:        "mid_quality_0.2_to_0.4_below",
			actualScore: 0.1,
			minScore:    0.2,
			maxScore:    0.4,
			wantGrade:   0.75, // 1 - (0.1 / (0.2 * 2)) = 1 - (0.1 / 0.4) = 0.75
		}, {
			name:        "mid_quality_0.3_to_0.6_inside",
			actualScore: 0.45,
			minScore:    0.3,
			maxScore:    0.6,
			wantGrade:   1.0,
		}, {
			name:        "mid_quality_0.3_to_0.6_above",
			actualScore: 0.9,
			minScore:    0.3,
			maxScore:    0.6,
			wantGrade:   0.5, // 1 - (0.3 / (0.3 * 2)) = 1 - (0.3 / 0.6) = 0.5
		}, {
			name:        "mid_quality_0.5_to_0.7_inside",
			actualScore: 0.6,
			minScore:    0.5,
			maxScore:    0.7,
			wantGrade:   1.0,
		}, {
			name:        "mid_quality_0.5_to_0.7_below",
			actualScore: 0.3,
			minScore:    0.5,
			maxScore:    0.7,
			wantGrade:   0.5, // 1 - (0.2 / (0.2 * 2)) = 1 - (0.2 / 0.4) = 0.5
		},
		// High-quality ranges (from integration tests)
		{
			name:        "high_quality_0.7_to_0.9_inside",
			actualScore: 0.8,
			minScore:    0.7,
			maxScore:    0.9,
			wantGrade:   1.0,
		}, {
			name:        "high_quality_0.7_to_0.9_below",
			actualScore: 0.5,
			minScore:    0.7,
			maxScore:    0.9,
			wantGrade:   0.5, // 1 - (0.2 / (0.2 * 2)) = 1 - (0.2 / 0.4) = 0.5
		}, {
			name:        "high_quality_0.9_to_1.0_inside",
			actualScore: 0.95,
			minScore:    0.9,
			maxScore:    1.0,
			wantGrade:   1.0,
		}, {
			name:        "high_quality_0.9_to_1.0_below",
			actualScore: 0.8,
			minScore:    0.9,
			maxScore:    1.0,
			wantGrade:   0.5, // 1 - (0.1 / (0.1 * 2)) = 1 - (0.1 / 0.2) = 0.5
		},
		// Wide ranges
		{
			name:        "full_range_0_to_1_inside",
			actualScore: 0.5,
			minScore:    0.0,
			maxScore:    1.0,
			wantGrade:   1.0,
		}, {
			name:        "full_range_0_to_1_outside_impossible",
			actualScore: 1.5,
			minScore:    0.0,
			maxScore:    1.0,
			wantGrade:   0.75, // 1 - (0.5 / (1.0 * 2)) = 1 - (0.5 / 2.0) = 0.75
		},
		// Edge cases with very small values
		{
			name:        "tiny_range_0.001_to_0.002",
			actualScore: 0.0015,
			minScore:    0.001,
			maxScore:    0.002,
			wantGrade:   1.0,
		}, {
			name:        "tiny_range_0.001_to_0.002_outside",
			actualScore: 0.003,
			minScore:    0.001,
			maxScore:    0.002,
			wantGrade:   0.5, // 1 - (0.001 / (0.001 * 2)) = 1 - (0.001 / 0.002) = 0.5
		},
		// Edge cases with values near 1.0
		{
			name:        "near_max_0.98_to_0.99",
			actualScore: 0.985,
			minScore:    0.98,
			maxScore:    0.99,
			wantGrade:   1.0,
		}, {
			name:        "near_max_0.98_to_0.99_outside",
			actualScore: 1.0,
			minScore:    0.98,
			maxScore:    0.99,
			wantGrade:   0.5, // 1 - (0.01 / (0.01 * 2)) = 1 - (0.01 / 0.02) = 0.5
		},
		// Asymmetric distance cases
		{
			name:        "asymmetric_far_below",
			actualScore: 0.1,
			minScore:    0.8,
			maxScore:    0.9,
			wantGrade:   0.0, // 1 - (0.7 / (0.1 * 2)) = 1 - (0.7 / 0.2) = 1 - 3.5 = 0 (capped)
		}, {
			name:        "asymmetric_far_above",
			actualScore: 0.99,
			minScore:    0.1,
			maxScore:    0.2,
			wantGrade:   0.0, // 1 - (0.79 / (0.1 * 2)) = 1 - (0.79 / 0.2) = 1 - 3.95 = 0 (capped)
		}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grade := calculateRangeGrade(tt.actualScore, tt.minScore, tt.maxScore)

			// Verify grade is within bounds
			if grade < 0.0 || grade > 1.0 {
				t.Errorf("grade out of bounds: got = %f, wanted = [0.0, 1.0]", grade)
			}

			// Check if grade is approximately equal to expected (within tolerance for floating point)
			if abs(grade-tt.wantGrade) > 0.001 {
				t.Errorf("grade: got = %f, wanted = %f", grade, tt.wantGrade)
			}
		})
	}
}

func TestScoreRange_BenchmarkNegativeValues(t *testing.T) {
	tests := []struct {
		name        string
		actualScore float64
		minScore    float64
		maxScore    float64
		wantGrade   float64
	}{{
		name:        "benchmark_negative_range_inside",
		actualScore: -0.5,
		minScore:    -1.0,
		maxScore:    0.0,
		wantGrade:   1.0,
	}, {
		name:        "benchmark_negative_range_below",
		actualScore: -1.5,
		minScore:    -1.0,
		maxScore:    0.0,
		wantGrade:   0.75, // 1 - (0.5 / (1.0 * 2)) = 1 - (0.5 / 2.0) = 0.75
	}, {
		name:        "benchmark_full_range_inside",
		actualScore: 0.0,
		minScore:    -1.0,
		maxScore:    1.0,
		wantGrade:   1.0,
	}, {
		name:        "benchmark_negative_inside",
		actualScore: -0.3,
		minScore:    -0.5,
		maxScore:    0.8,
		wantGrade:   1.0,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grade := calculateRangeGrade(tt.actualScore, tt.minScore, tt.maxScore)

			if grade < 0.0 || grade > 1.0 {
				t.Errorf("grade out of bounds: got = %f, wanted = [0.0, 1.0]", grade)
			}

			if abs(grade-tt.wantGrade) > 0.001 {
				t.Errorf("grade: got = %f, wanted = %f", grade, tt.wantGrade)
			}
		})
	}
}

func TestScoreRange_NilResult(t *testing.T) {
	observer := &mockObserver{}

	ScoreRange(0.5, 0.8)(observer, &evals.Trace[*Judgement]{
		Result: nil,
	})

	// Should fail when result is nil
	if len(observer.failures) != 1 {
		t.Errorf("failures: got = %d, wanted = 1", len(observer.failures))
	}

	if len(observer.failures) > 0 && observer.failures[0] != "judgment result is nil" {
		t.Errorf("failure message: got = %q, wanted = %q", observer.failures[0], "judgment result is nil")
	}

	// Should not call Grade when result is nil
	if len(observer.grades) != 0 {
		t.Errorf("grades: got = %d, wanted = 0", len(observer.grades))
	}
}

// roundToIncrement rounds a float64 to the nearest increment to handle floating point precision issues
func roundToIncrement(value, increment float64) float64 {
	return float64(int(value/increment+0.5)) * increment
}
