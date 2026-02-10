/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"fmt"
	"strings"
)

// JudgmentMode specifies the type of judgment to perform.
type JudgmentMode string

const (
	// GoldenMode evaluates a response against a reference answer.
	GoldenMode JudgmentMode = "golden"
	// BenchmarkMode compares two responses to determine which is better.
	BenchmarkMode JudgmentMode = "benchmark"
	// StandaloneMode evaluates a single response against a criterion without a reference.
	StandaloneMode JudgmentMode = "standalone"
)

// Request contains the context for judgment
type Request struct {
	// Mode specifies the judgment mode.
	Mode JudgmentMode `json:"mode"`

	// ReferenceAnswer is the golden answer to compare against.
	ReferenceAnswer string `json:"reference_answer,omitempty"`

	// ActualAnswer is the answer to evaluate.
	ActualAnswer string `json:"actual_answer"`

	// Criterion specifies the evaluation criterion.
	Criterion string `json:"criterion"`
}

// Judgement contains the judgment result
type Judgement struct {
	// Mode is the judgment mode used. Available in agenttrace.Trace for mode-specific processing.
	Mode JudgmentMode `json:"mode"`

	// Score is the primary judgment metric from 0.0 (awful) to 1.0 (ideal - matches golden answer).
	Score float64 `json:"score"`

	// Reasoning explains the judgment and score.
	Reasoning string `json:"reasoning"`

	// Suggestions provides improvement recommendations. May be empty for perfect scores.
	Suggestions []string `json:"suggestions"`
}

// String returns a formatted representation of the judgment similar to trace output
func (j *Judgement) String() string {
	var sb strings.Builder

	// Header with score
	sb.WriteString(fmt.Sprintf("Grade: %.2f", j.Score))

	// Add reasoning if present
	if j.Reasoning != "" {
		sb.WriteString(fmt.Sprintf(" - %s", j.Reasoning))
	}
	sb.WriteString("\n")

	// Add suggestions if present
	if len(j.Suggestions) > 0 {
		for _, suggestion := range j.Suggestions {
			sb.WriteString(fmt.Sprintf("  Suggestion: %s\n", suggestion))
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// Interface defines the contract for judge implementations
type Interface interface {
	// Judge evaluates an actual response against a golden answer using the provided rubric
	Judge(ctx context.Context, request *Request) (*Judgement, error)
}
