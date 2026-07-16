/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package telemetry_test

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
)

func ExampleNewRecorder() {
	rec := telemetry.NewRecorder(
		metrics.NewGenAI("chainguard.ai.agents"),
		"claude-sonnet-4",
		"anthropic",
		map[string]string{"team": "platform"},
		func(error) int { return -1 },
	)
	rec.RecordTokens(context.Background(), 150, 250)
	fmt.Println("token metrics recorded")
	// Output:
	// token metrics recorded
}

func ExampleRecorder() {
	ctx := context.Background()
	rec := telemetry.NewRecorder(
		metrics.NewGenAI("chainguard.ai.agents"),
		"gemini-2.5-flash",
		"gcp.vertex_ai",
		nil,
		func(error) int { return -1 },
	)
	rec.RecordCacheTokens(ctx, 1024, 0)
	rec.RecordToolCall(ctx, "read_file")
	rec.RecordTurns(ctx, 3, false)
	fmt.Println("execution metrics recorded")
	// Output:
	// execution metrics recorded
}

func ExampleRecorder_RecordAPIRequest() {
	ctx := context.Background()
	rec := telemetry.NewRecorder(
		metrics.NewGenAI("chainguard.ai.agents"),
		"gemini-2.5-flash",
		"gcp.vertex_ai",
		nil,
		func(err error) int {
			if err == nil {
				return 0
			}
			return 429
		},
	)
	// A nil error counts as response_code "200"; failures carry the code the
	// mapping recovers from the error.
	rec.RecordAPIRequest(ctx, nil)
	rec.RecordAPIRequest(ctx, errors.New("rate limited"))
	fmt.Println("api request metrics recorded")
	// Output:
	// api request metrics recorded
}

func ExampleRecorder_WithAPIRequestCounter() {
	ctx := context.Background()
	rec := telemetry.NewRecorder(
		metrics.NewGenAI("chainguard.ai.agents"),
		"gemini-2.5-flash",
		"gcp.vertex_ai",
		nil,
		func(error) int { return 503 },
	)
	// Wrap a retry config so every retried attempt is counted in
	// genai.api.requests alongside the final attempt.
	cfg := rec.WithAPIRequestCounter(ctx, retry.DefaultRetryConfig())
	cfg.OnAttemptError(errors.New("transient"))
	fmt.Println("retried attempt counted")
	// Output:
	// retried attempt counted
}
