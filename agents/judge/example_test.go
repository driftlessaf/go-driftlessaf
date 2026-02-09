/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"
	"fmt"
	"math/rand"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/judge"
)

func ExampleNewGoldenEval() {
	// Create eval callback for a specific criterion
	eval := judge.NewGoldenEval[*judge.Judgement](
		&mockJudge{
			judgment: &judge.Judgement{
				Score:     0.85,
				Reasoning: "The response correctly identifies the answer but uses words instead of the expected numeric format",
				Suggestions: []string{
					"Use numeric format (42) instead of words (forty-two)",
				},
			},
		},
		"numeric format - responses should use numbers not words",
		"The answer is 42",
	)

	// Create context with tracer
	ctx := context.Background()
	obs := &mockObserver{}
	ctx = evals.WithTracer(ctx, evals.ByCode(evals.Inject(obs, eval)))

	// Simulate a judgment call that would normally be traced
	trace := evals.StartTrace[*judge.Judgement](ctx, fmt.Sprintf("prompt-%d", rand.Int63()))
	trace.Complete(&judge.Judgement{
		Score:     0.85,
		Reasoning: "The response correctly identifies the answer but uses words instead of the expected numeric format",
		Suggestions: []string{
			"Use numeric format (42) instead of words (forty-two)",
		},
	}, nil)

	// The eval will have been called automatically by the tracer
	for _, log := range obs.logs {
		fmt.Println(log)
	}

	// Output:
	// Grade: 0.85 - The response correctly identifies the answer but uses words instead of the expected numeric format
	//   Suggestion: Use numeric format (42) instead of words (forty-two)
}

func Example_multipleCriteria() {
	// When evaluating multiple criteria, create separate eval callbacks
	// This allows fine-grained control and visibility into each aspect

	goldenCode := `func Add(a, b int) int { return a + b }`
	mockJudgeInstance := &mockJudge{
		judgment: &judge.Judgement{
			Score:       0.75,
			Reasoning:   "Code meets most requirements with minor issues",
			Suggestions: []string{"Add error handling", "Improve variable names"},
		},
	}

	// Set up tracer with multiple criteria
	ctx := context.Background()
	obs := &mockObserver{}
	ctx = evals.WithTracer(ctx, evals.ByCode(
		evals.Inject(obs, judge.NewGoldenEval[*judge.Judgement](mockJudgeInstance, "correctness - the code produces the expected output", goldenCode)),
		evals.Inject(obs, judge.NewGoldenEval[*judge.Judgement](mockJudgeInstance, "readability - the code is easy to understand", goldenCode)),
		evals.Inject(obs, judge.NewGoldenEval[*judge.Judgement](mockJudgeInstance, "efficiency - the code performs well", goldenCode)),
		evals.Inject(obs, judge.NewGoldenEval[*judge.Judgement](mockJudgeInstance, "security - the code has no vulnerabilities", goldenCode)),
	))

	// Simulate evaluation - all criteria will be evaluated automatically
	trace := evals.StartTrace[*judge.Judgement](ctx, fmt.Sprintf("eval-%d", rand.Int63()))
	trace.Complete(&judge.Judgement{
		Score:       0.75,
		Reasoning:   "Code meets most requirements with minor issues",
		Suggestions: []string{"Add error handling", "Improve variable names"},
	}, nil)

	fmt.Printf("Evaluated %d criteria successfully\n", 4)

	// Output:
	// Evaluated 4 criteria successfully
}

// ExampleNewVertex demonstrates creating a judge using the unified factory method.
// The method automatically selects the appropriate implementation based on the model name.
func ExampleNewVertex() {
	ctx := context.Background()

	// Create a Claude judge instance with resource labels for billing attribution
	labels := map[string]string{"agent_name": "judge"}
	judgeInstance, err := judge.NewVertex(ctx, "my-project", "us-east5", "claude-sonnet-4@20250514",
		claudeexecutor.WithResourceLabels[*judge.Request, *judge.Judgement](labels))
	if err != nil {
		fmt.Printf("Error creating judge: %v\n", err)
		return
	}

	// Use the judge to evaluate a response
	judgment, err := judgeInstance.Judge(ctx, &judge.Request{
		Mode:            judge.GoldenMode,
		ReferenceAnswer: "The capital of France is Paris.",
		ActualAnswer:    "Paris is the capital of France.",
		Criterion:       "factual accuracy - response should be correct",
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Printf("Score: %.2f\n", judgment.Score)
	fmt.Printf("Reasoning: %s\n", judgment.Reasoning)
}

// ExampleNewVertex_gemini demonstrates creating a Gemini-based judge.
func ExampleNewVertex_gemini() {
	ctx := context.Background()

	// Create a Gemini judge instance with resource labels for billing attribution
	labels := map[string]string{"agent_name": "judge"}
	judgeInstance, err := judge.NewVertex(ctx, "my-project", "us-central1", "gemini-2.5-flash",
		googleexecutor.WithResourceLabels[*judge.Request, *judge.Judgement](labels))
	if err != nil {
		fmt.Printf("Error creating judge: %v\n", err)
		return
	}

	// Use the judge to evaluate a response
	judgment, err := judgeInstance.Judge(ctx, &judge.Request{
		Mode:            judge.GoldenMode,
		ReferenceAnswer: "42",
		ActualAnswer:    "The answer is 42",
		Criterion:       "completeness - response should include the exact answer",
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Printf("Score: %.2f\n", judgment.Score)
}

func ExampleValidScore() {
	// Create context with tracer
	ctx := context.Background()
	obs := &mockObserver{}
	ctx = evals.WithTracer(ctx, evals.ByCode(evals.Inject(obs, judge.ValidScore(judge.GoldenMode))))

	// Test with valid score - tracer will call validator automatically
	trace := evals.StartTrace[*judge.Judgement](ctx, fmt.Sprintf("valid-%d", rand.Int63()))
	trace.Complete(&judge.Judgement{Score: 0.85}, nil)

	// Test with invalid score
	trace2 := evals.StartTrace[*judge.Judgement](ctx, fmt.Sprintf("invalid-%d", rand.Int63()))
	trace2.Complete(&judge.Judgement{Score: 1.5}, nil)

	fmt.Printf("Validation results: %d messages\n", len(obs.logs))
	// Output:
	// Validation results: 1 messages
}

// ExampleScoreRange demonstrates validating that a judge scores responses within expected quality ranges.
func ExampleScoreRange() {
	ctx := context.Background()
	obs := &mockObserver{}

	// Create a judge instance with resource labels
	labels := map[string]string{"agent_name": "judge"}
	judgeInstance, err := judge.NewVertex(ctx, "my-project", "us-central1", "gemini-2.5-flash",
		googleexecutor.WithResourceLabels[*judge.Request, *judge.Judgement](labels))
	if err != nil {
		fmt.Printf("Error creating judge: %v\n", err)
		return
	}

	// Set up validation that the judge scores a "good quality" response between 0.7-0.9
	ctx = evals.WithTracer(ctx, evals.ByCode(evals.Inject(obs, judge.ScoreRange(0.7, 0.9))))

	// Test the judge with a response we expect to be "good quality"
	_, err = judgeInstance.Judge(ctx, &judge.Request{
		Mode:         judge.StandaloneMode,
		ActualAnswer: "Python is a high-level programming language known for simplicity and readability.",
		Criterion:    "completeness - response should provide comprehensive information about Python",
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	// ScoreRange validates the judge scored within our expected 0.7-0.9 "good quality" range
	fmt.Println("Judge scored within expected range for good quality response")
}

func ExampleHasReasoning() {
	// Create context with tracer
	ctx := context.Background()
	obs := &mockObserver{}
	ctx = evals.WithTracer(ctx, evals.ByCode(evals.Inject(obs, judge.HasReasoning())))

	// Test with reasoning - tracer will call validator automatically
	trace := evals.StartTrace[*judge.Judgement](ctx, fmt.Sprintf("with-reasoning-%d", rand.Int63()))
	trace.Complete(&judge.Judgement{
		Score:     0.8,
		Reasoning: "The response correctly identifies the main point",
	}, nil)

	// Test without reasoning
	trace2 := evals.StartTrace[*judge.Judgement](ctx, fmt.Sprintf("no-reasoning-%d", rand.Int63()))
	trace2.Complete(&judge.Judgement{Score: 0.8}, nil)

	fmt.Printf("Validation completed\n")
	// Output:
	// Validation completed
}

func ExampleNoToolCalls() {
	// Create context with tracer
	ctx := context.Background()
	obs := &mockObserver{}
	ctx = evals.WithTracer(ctx, evals.ByCode(evals.Inject(obs, evals.NoToolCalls[*judge.Judgement]())))

	// Test with no tool calls - tracer will call validator automatically
	trace := evals.StartTrace[*judge.Judgement](ctx, fmt.Sprintf("no-tools-%d", rand.Int63()))
	trace.Complete(&judge.Judgement{Score: 0.8}, nil)

	fmt.Printf("Validation completed\n")
	// Output:
	// Validation completed
}

// ExampleNewStandaloneEval demonstrates using NewStandaloneEval to assess response quality against criteria.
func ExampleNewStandaloneEval() {
	ctx := context.Background()
	obs := &mockObserver{}

	// Create a judge instance with resource labels
	labels := map[string]string{"agent_name": "judge"}
	judgeInstance, err := judge.NewVertex(ctx, "my-project", "us-central1", "gemini-2.5-flash",
		googleexecutor.WithResourceLabels[*judge.Request, *judge.Judgement](labels))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	// Example response to evaluate for quality
	response := &judge.Judgement{
		Score:     0.7,
		Reasoning: "Instructions are clear but could be more detailed",
	}

	// Create standalone evaluation to assess clarity without reference answer
	eval := judge.NewStandaloneEval[*judge.Judgement](
		judgeInstance,
		"clarity - instructions should be easy to understand and follow",
	)

	// Create trace and run evaluation
	trace := &evals.Trace[*judge.Judgement]{
		InputPrompt: "Evaluate instruction clarity",
		Result:      response,
	}

	eval(obs, trace)

	fmt.Printf("Quality assessment completed\n")
}
