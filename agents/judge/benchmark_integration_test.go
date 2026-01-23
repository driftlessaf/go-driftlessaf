//go:build withauth

/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"
	"maps"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/evals/report"
	"chainguard.dev/driftlessaf/agents/evals/testevals"
	"chainguard.dev/driftlessaf/agents/judge"
)

func TestBenchmark(t *testing.T) {
	ctx := context.Background()

	// Try to detect project ID from multiple sources
	projectID := detectProjectID(ctx, t)

	threshold := 0.8 // 80% success rate threshold

	// Create meta-judge instance for LLM-as-a-judge evaluation using Gemini Flash
	metaJudgeInstance, err := judge.NewVertex(ctx, projectID, "us-central1", "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("Failed to create meta-judge instance: %v", err)
	}

	// Create namespaced observer using testevals.NewPrefix wrapped in ResultCollector
	obs := evals.NewNamespacedObserver(func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(testevals.NewPrefix(t, name))
	})

	// Define base model test configurations (Gemini models for fast testing)
	tests := []struct {
		name   string
		region string
		model  string
	}{{
		name:   "gemini-2.5-flash",
		region: "us-central1",
		// https://cloud.google.com/vertex-ai/generative-ai/docs/models/gemini/2-5-flash
		model: "gemini-2.5-flash",
	}, {
		name:   "gemini-2.5-flash-lite",
		region: "us-central1",
		// https://cloud.google.com/vertex-ai/generative-ai/docs/models/gemini/2-5-flash-lite
		model: "gemini-2.5-flash-lite",
	}}

	// Add Claude models when not in short mode
	if !testing.Short() {
		tests = append(tests, []struct {
			name   string
			region string
			model  string
		}{{
			name:   "opus-4-5",
			region: "us-east5",
			// https://cloud.google.com/vertex-ai/generative-ai/docs/partner-models/claude/opus-4-5
			model: "claude-opus-4-5@20251101",
		}, {
			name:   "sonnet-4",
			region: "us-east5",
			// https://cloud.google.com/vertex-ai/generative-ai/docs/partner-models/claude/sonnet-4
			model: "claude-sonnet-4@20250514",
		}}...)
	}

	// createEvals creates all evaluation callbacks for benchmark mode validation
	createEvals := func(minScore, maxScore float64, golden string) map[string]evals.ObservableTraceCallback[*judge.Judgement] {
		// Start with standard benchmark evaluations
		evalMap := judge.Evals(judge.BenchmarkMode)

		// Add range-specific evaluations using standard eval helpers
		maps.Copy(evalMap, map[string]evals.ObservableTraceCallback[*judge.Judgement]{
			"score-range": judge.ScoreRange(minScore, maxScore),
		})

		// Add judge-based meta-evaluations for benchmark mode
		maps.Copy(evalMap, map[string]evals.ObservableTraceCallback[*judge.Judgement]{
			"judge-reasoning": judge.NewGoldenEval[*judge.Judgement](
				metaJudgeInstance,
				"comparative reasoning accuracy - Evaluate whether the 'reasoning' field correctly explains the comparative assessment and justifies the score given. The reasoning should clearly articulate which response is better and why, comparing specific aspects like completeness, accuracy, clarity, or effectiveness. Focus on whether the comparative logic is sound, not whether it matches a specific reasoning style.",
				golden,
			),
			"judge-suggestions": judge.NewGoldenEval[*judge.Judgement](
				metaJudgeInstance,
				"suggestion quality and appropriateness - Evaluate whether suggestions are useful and actionable when provided. For equivalent responses (score 0.0), suggestions are not required but may highlight different approaches or strengths. For non-equivalent responses, suggestions should target actual weaknesses in the lower-performing response with specific, actionable improvements. Focus on quality and usefulness of suggestions provided, not whether suggestions are present.",
				golden,
			),
		})

		return evalMap
	}

	t.Logf("Running %d test configurations (testing.Short=%v)", len(tests), testing.Short())

	wg := sync.WaitGroup{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create judge instance using unified factory method
			judgeInstance, err := judge.NewVertex(ctx, projectID, tt.region, tt.model)
			if err != nil {
				t.Fatalf("Failed to create judge: %v", err)
			}

			// Log the configuration being tested
			t.Logf("Testing benchmark judge with region=%s, model=%s", tt.region, tt.model)

			// Create namespaced observer for this model
			modelObs := obs.Child(tt.name)

			testCases := []struct {
				Name      string
				Foo       string // First response (foo)
				Bar       string // Second response (bar)
				Criterion string
				Evals     map[string]evals.ObservableTraceCallback[*judge.Judgement]
			}{{
				Name:      "clear_foo_winner",
				Foo:       "The capital of France is Paris.",
				Bar:       "The capital of France is Lisbon.",
				Criterion: "completeness and accuracy - response should provide complete, accurate information",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo provides comprehensive, accurate information while bar is completely wrong - Lisbon is Portugal's capital",
  "suggestions": [
    "Correct the factual error - France's capital is Paris, not Lisbon",
    "Verify geographical facts before responding"
  ]
}`),
			}, {
				Name:      "clear_bar_winner",
				Foo:       "I think it's London or something.",
				Bar:       "The capital of France is Paris.",
				Criterion: "factual accuracy - response must be correct",
				Evals: createEvals(1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "Bar provides correct factual information while foo is completely wrong",
  "suggestions": [
    "Correct the factual error - France's capital is Paris, not London"
  ]
}`),
			}, {
				Name:      "roughly_equivalent",
				Foo:       "The Earth revolves around the Sun.",
				Bar:       "The Earth orbits the Sun.",
				Criterion: "scientific accuracy - response must be scientifically correct",
				Evals: createEvals(-0.1, 0.1, `{
  "score": 0.0,
  "reasoning": "Both responses are scientifically correct with equivalent meaning - 'revolves' and 'orbits' describe the same astronomical relationship",
  "suggestions": []
}`),
			}, {
				Name:      "identical_responses",
				Foo:       "The capital of France is Paris.",
				Bar:       "The capital of France is Paris.",
				Criterion: "accuracy - response should provide correct factual information",
				Evals: createEvals(0.0, 0.0, `{
  "score": 0.0,
  "reasoning": "Both responses are identical - no difference in quality, content, or effectiveness",
  "suggestions": []
}`),
			}, {
				Name:      "foo_somewhat_better",
				Foo:       "To reset your password: 1) Click 'Forgot Password' 2) Enter your email 3) Check your inbox 4) Follow the link to create new password",
				Bar:       "Click 'Forgot Password' and enter your email.",
				Criterion: "completeness - instructions should include all necessary steps",
				Evals: createEvals(-1.0, -0.8, `{
  "score": -0.9,
  "reasoning": "Foo significantly outperforms bar in completeness by providing a comprehensive 4-step password reset process, while bar only covers the initial two steps and omits crucial follow-up actions like checking the inbox and following the reset link, making it incomplete and potentially confusing",
  "suggestions": [
    "Add step 3: 'Check your inbox for the password reset email'",
    "Add step 4: 'Follow the link to create new password'",
    "Consider numbering steps for clarity"
  ]
}`),
			}, {
				Name:      "bar_somewhat_better",
				Foo:       "The server returns a 404 error.",
				Bar:       "HTTP 404 indicates that the requested resource was not found on the server.",
				Criterion: "technical detail - response should explain the technical concept clearly",
				Evals: createEvals(0.65, 0.85, `{
  "score": 0.75,
  "reasoning": "Bar provides clear technical explanation while foo just states the error without explaining what it means",
  "suggestions": [
    "Explain what 404 errors mean",
    "Add technical details about server response"
  ]
}`),
			}, {
				Name:      "equivalent_customer_messages",
				Foo:       "Customer satisfaction is our highest priority.",
				Bar:       "We prioritize our customers above all else.",
				Criterion: "message consistency - response should convey the company's customer-first values",
				Evals: createEvals(0.0, 0.0, `{
  "score": 0.0,
  "reasoning": "Both responses effectively convey the same customer-first commitment using different but equally valid phrasing",
  "suggestions": []
}`),
			}, {
				Name:      "exact_match_vs_different",
				Foo:       "The capital of France is Paris.",
				Bar:       "The capital of France is Lyon.",
				Criterion: "factual accuracy - response must be correct",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo is factually correct while bar is completely wrong - Lyon is not France's capital",
  "suggestions": [
    "Correct the factual error - France's capital is Paris, not Lyon"
  ]
}`),
			}, {
				Name:      "semantic_equivalence",
				Foo:       "The answer is 42",
				Bar:       "42 is the answer",
				Criterion: "semantic equivalence - responses should convey the same meaning",
				Evals: createEvals(-0.1, 0.1, `{
  "score": 0.0,
  "reasoning": "Both responses convey identical meaning with semantic equivalence - just different word order",
  "suggestions": []
}`),
			}, {
				Name:      "completeness_complete_vs_partial",
				Foo:       "Python is a high-level, interpreted programming language known for its simplicity and readability.",
				Bar:       "Python is a programming language.",
				Criterion: "completeness - response should include all key information",
				Evals: createEvals(-1.0, -0.8, `{
  "score": -0.9,
  "reasoning": "Foo provides a significantly more complete description by explicitly listing key characteristics (high-level, interpreted, simplicity, readability) that distinguish Python, while bar offers only a bare minimum description lacking these distinguishing features, making it much less informative and complete",
  "suggestions": [
    "Add that Python is a high-level programming language",
    "Specify that Python is an interpreted language",
    "Add information about Python's simplicity and readability"
  ]
}`),
			}, {
				Name:      "format_compliance_vs_violation",
				Foo:       "42",
				Bar:       "forty-two",
				Criterion: "numeric format - responses must be strictly numeric digits only",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo uses required numeric format while bar violates the strict numeric requirement",
  "suggestions": [
    "Use numeric digits (42) instead of spelling out the number in words"
  ]
}`),
			}, {
				Name:      "professional_tone_good_vs_poor",
				Foo:       "We appreciate your feedback and will address this issue promptly.",
				Bar:       "Yeah, we got your complaint and we'll fix it when we get around to it.",
				Criterion: "professional tone - response should maintain formal, respectful language",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo maintains professional tone while bar is unprofessional, casual, and dismissive",
  "suggestions": [
    "Use formal language instead of casual expressions like 'yeah'",
    "Replace dismissive phrasing with committed language",
    "Show appreciation for feedback rather than calling it a 'complaint'"
  ]
}`),
			}, {
				Name:      "technical_accuracy_correct_vs_incorrect",
				Foo:       "HTTP 404 indicates that the requested resource was not found on the server.",
				Bar:       "404 means your internet connection is broken.",
				Criterion: "technical accuracy - response must correctly explain technical concepts",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo correctly explains 404 errors while bar is completely wrong - 404 is server-side, not connectivity",
  "suggestions": [
    "Correct the explanation: 404 is about missing resources, not connectivity",
    "Distinguish between client-side and server-side errors",
    "Explain that 404 means 'resource not found on server'"
  ]
}`),
			}, {
				Name:      "conciseness_good_vs_verbose",
				Foo:       "The meeting is at 3 PM.",
				Bar:       "I would like to inform you that the upcoming meeting that we have scheduled is going to take place at the time of 3 PM in the afternoon.",
				Criterion: "conciseness - response should be brief while containing essential information",
				Evals: createEvals(-1.0, -0.7, `{
  "score": -0.9,
  "reasoning": "Foo is appropriately concise while bar is unnecessarily verbose with redundant information",
  "suggestions": [
    "Remove redundant phrases like 'I would like to inform you'",
    "Eliminate unnecessary details like 'in the afternoon' after PM",
    "Use direct, simple language"
  ]
}`),
			}, {
				Name:      "relevance_on_topic_vs_off_topic",
				Foo:       "The capital of France is Paris.",
				Bar:       "France is known for its wine and cuisine.",
				Criterion: "relevance - response must directly address the question asked",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo directly answers the question while bar is completely off-topic",
  "suggestions": [
    "Answer the specific question asked about France's capital",
    "Stay focused on the requested information rather than general facts"
  ]
}`),
			}, {
				Name:      "clarity_clear_vs_confusing",
				Foo:       "To reset your password, click the \"Forgot Password\" link on the login page.",
				Bar:       "There's a thing you can use that might help with authentication credential restoration if you access the entry portal interface.",
				Criterion: "clarity - instructions should be easy to understand and follow",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo provides clear, direct instructions while bar is confusing and uses unnecessarily complex language",
  "suggestions": [
    "Use simple, direct language instead of technical jargon",
    "Provide specific action items rather than vague descriptions",
    "Be explicit about what to click and where to find it"
  ]
}`),
			}, {
				Name:      "completeness_thorough_vs_incomplete",
				Foo:       "To bake a cake: combine 2 cups flour, 1 cup sugar, 2 eggs, and 1/2 cup butter; mix well; bake at 350°F for 30 minutes; cool before removing from pan.",
				Bar:       "Mix some ingredients and bake.",
				Criterion: "completeness - response should include all necessary steps and details",
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo provides comprehensive instructions with specific measurements and steps while bar is severely incomplete",
  "suggestions": [
    "Specify the required ingredients: flour, sugar, eggs, butter",
    "Include baking temperature: 350°F",
    "Provide baking time: 30 minutes",
    "Add mixing instructions"
  ]
}`),
			}, {
				Name: "consistency_aligned_vs_contradictory",
				Foo:  "Our company values customer satisfaction above all else.",
				Bar:  "Profit margins are our primary concern, though we do consider customer needs occasionally.",
				Criterion: `alignment with company values - response should align with the company values:
• Customer Obsessed
• We do serious work but don't take ourselves too seriously
• Trust each other and assume good intentions
• Bias for intentional action`,
				Evals: createEvals(-1.0, -1.0, `{
  "score": -1.0,
  "reasoning": "Foo aligns with company values while bar directly contradicts them by prioritizing profit over customers",
  "suggestions": [
    "Align messaging with stated company values",
    "Prioritize customer satisfaction in all communications",
    "Remove contradictory statements about profit being primary"
  ]
}`),
			}, {
				Name:      "empathy_understanding_vs_dismissive",
				Foo:       "I understand this situation is frustrating. Let me help you resolve it.",
				Bar:       "This is a simple issue. Just follow the standard procedure.",
				Criterion: "empathy - response should acknowledge customer emotions and show understanding",
				Evals: createEvals(-1.0, -0.8, `{
  "score": -1.0,
  "reasoning": "Foo demonstrates empathy and understanding while bar is dismissive and fails to acknowledge emotions",
  "suggestions": [
    "Acknowledge the customer's frustration before providing solutions",
    "Avoid minimizing their concerns by calling it 'simple'",
    "Show willingness to help rather than just directing to procedures"
  ]
}`),
			}}

			wg.Add(len(testCases))
			// Wait until we have added the test case count to allow things to proceed in parallel.
			t.Parallel()

			// Run each test case
			for _, tc := range testCases {
				t.Run(tc.Name, func(t *testing.T) {
					// Run each test case in parallel (this may offend quotas)
					t.Parallel()
					t.Cleanup(wg.Done)
					// Create a child observer for this test case
					testObs := modelObs.Child(tc.Name)

					// Create context with combined evals tracer
					testCtx := context.Background()
					testCtx = evals.WithTracer(testCtx, evals.BuildTracer(testObs, tc.Evals))

					// Call judge with benchmark mode and verify response via evals callbacks
					_, err := judgeInstance.Judge(testCtx, &judge.Request{
						Mode:            judge.BenchmarkMode,
						ReferenceAnswer: tc.Foo,
						ActualAnswer:    tc.Bar,
						Criterion:       tc.Criterion,
					})
					if err != nil {
						t.Fatalf("Error running benchmark judge: %v", err)
					}
				})
			}
		})
	}

	// Cleanup function to wait for all parallel tests and generate report
	// If we don't do this in Cleanup, then things seem to block forever!
	t.Cleanup(func() {
		// Wait for all the parallel tests to finish
		wg.Wait()

		// Generate ByEval report for comparison
		byEvalReportStr, byEvalHasFailure := report.ByEval(obs, threshold)

		// Print the ByEval report
		if byEvalHasFailure {
			t.Errorf("\nByEval Report (FAILED - some evaluations below %.0f%% threshold):\n\n%s", threshold*100, byEvalReportStr)
		} else {
			t.Logf("\nByEval Report (PASSED - all evaluations above %.0f%% threshold):\n\n%s", threshold*100, byEvalReportStr)
		}
	})
}
