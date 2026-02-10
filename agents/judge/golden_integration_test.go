//go:build withauth

/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"context"
	"maps"
	"os"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/evals/report"
	"chainguard.dev/driftlessaf/agents/evals/testevals"
	"chainguard.dev/driftlessaf/agents/judge"
	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2/google"
)

func TestGolden(t *testing.T) {
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

	// createEvals creates all evaluation callbacks including global and test-specific ones
	createEvals := func(min, max float64, golden string) map[string]evals.ObservableTraceCallback[*judge.Judgement] {
		// Start with standard judge evaluations
		evalMap := judge.Evals(judge.GoldenMode)

		// Add test-specific evaluations
		maps.Copy(evalMap, map[string]evals.ObservableTraceCallback[*judge.Judgement]{
			"score-range": judge.ScoreRange(min, max),
			"judge-reasoning": judge.NewGoldenEval[*judge.Judgement](
				metaJudgeInstance,
				// Evaluation criteria: semantic accuracy of reasoning
				"semantic accuracy of reasoning - Evaluate whether the 'reasoning' field correctly explains and justifies the score given. The actual reasoning may be more detailed than the golden answer. Additional accurate details should not be penalized. Focus on whether the core justification is correct, not whether it matches the golden answer's level of detail.",
				golden,
			),
			"judge-suggestions": judge.NewGoldenEval[*judge.Judgement](
				metaJudgeInstance,
				// Evaluation criteria: appropriateness of suggestions
				"appropriateness of suggestions - Evaluate whether the 'suggestions' array follows the scoring rubric guidance. The suggestions should match the score given: 1.0 scores require empty array, 0.75-0.99 scores need specific minor improvements that justify the deduction, 0.50-0.74 scores need improvements addressing notable gaps, and lower scores need comprehensive improvements. Avoid redundant or overly general suggestions.",
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
			t.Logf("Testing judge with region=%s, model=%s", tt.region, tt.model)

			// Create namespaced observer for this model
			modelObs := obs.Child(tt.name)

			testCases := []struct {
				Name            string
				ReferenceAnswer string
				ActualAnswer    string
				Criterion       string
				Evals           map[string]evals.ObservableTraceCallback[*judge.Judgement] // Additional evaluation callbacks
			}{{
				Name:            "exact_match",
				ReferenceAnswer: "The capital of France is Paris.",
				ActualAnswer:    "The capital of France is Paris.",
				Criterion:       "exact match - the response should match the golden answer exactly",
				Evals: createEvals(1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "The actual response matches the golden answer exactly",
  "suggestions": []
}`),
			}, {
				Name:            "semantic_match",
				ReferenceAnswer: "The answer is 42",
				ActualAnswer:    "42 is the answer",
				Criterion:       "semantic equivalence - the response should convey the same meaning",
				Evals: createEvals(1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "The responses achieve the same quality and effectiveness - both identify 42 as the answer with semantic equivalence",
  "suggestions": []
}`),
			}, {
				Name:            "partial_match",
				ReferenceAnswer: "Python is a high-level, interpreted programming language known for its simplicity and readability.",
				ActualAnswer:    "Python is a programming language.",
				Criterion:       "completeness - the response should include all key information",
				Evals: createEvals(0.25, 0.45, `{
  "score": 0.35,
  "reasoning": "The actual response correctly identifies Python as a programming language but omits important details about it being high-level, interpreted, and known for simplicity and readability.",
  "suggestions": [
    "Include that Python is high-level",
    "Mention that Python is interpreted",
    "Add information about Python's simplicity and readability"
  ]
}`),
			}, {
				Name:            "format_criterion",
				ReferenceAnswer: "42",
				ActualAnswer:    "forty-two",
				Criterion:       "numeric format - responses must be strictly numeric digits only. This is a binary pass/fail evaluation: score 1.0 for purely numeric responses, score 0.0 for any non-numeric content.",
				Evals: createEvals(0.0, 0.2, `{
  "score": 0.0,
  "reasoning": "The actual response uses words (forty-two) instead of the required numeric format (42).",
  "suggestions": [
    "Use numeric digits (42) instead of spelling out the number in words"
  ]
}`),
			}, {
				Name:            "factual_accuracy_correct",
				ReferenceAnswer: "The Earth revolves around the Sun.",
				ActualAnswer:    "The Earth orbits the Sun.",
				Criterion:       "factual accuracy - response must be scientifically correct",
				Evals: createEvals(0.8, 1.0, `{
  "score": 0.95,
  "reasoning": "The actual response is factually correct. 'Orbits' and 'revolves around' have the same scientific meaning.",
  "suggestions": []
}`),
			}, {
				Name:            "factual_accuracy_incorrect",
				ReferenceAnswer: "The Earth revolves around the Sun.",
				ActualAnswer:    "The Sun revolves around the Earth.",
				Criterion:       "factual accuracy - response must be scientifically correct",
				Evals: createEvals(0.0, 0.1, `{
  "score": 0.0,
  "reasoning": "The actual response is factually incorrect. This represents the outdated geocentric model, not the correct heliocentric model.",
  "suggestions": [
    "Correct the relationship: Earth revolves around the Sun, not vice versa",
    "Review heliocentric vs geocentric models"
  ]
}`),
			}, {
				Name:            "tone_professional_good",
				ReferenceAnswer: "We appreciate your feedback and will address this issue promptly.",
				ActualAnswer:    "Your feedback has been received and the issue will be resolved.",
				Criterion:       "professional tone - response should maintain formal, respectful language",
				Evals: createEvals(0.7, 1.0, `{
  "score": 0.85,
  "reasoning": "The response maintains professional language and commits to resolving the issue. However, it lacks the warmth and appreciation shown in the reference, coming across as impersonal and somewhat robotic rather than genuinely engaging with the feedback provider.",
  "suggestions": [
    "Add acknowledgment of appreciation for the feedback to create a more engaging, less robotic tone"
  ]
}`),
			}, {
				Name:            "tone_professional_poor",
				ReferenceAnswer: "We appreciate your feedback and will address this issue promptly.",
				ActualAnswer:    "Yeah, we got your complaint and we'll fix it when we get around to it.",
				Criterion:       "professional tone - response should maintain formal, respectful language",
				Evals: createEvals(0.0, 0.3, `{
  "score": 0.1,
  "reasoning": "The actual response is unprofessional, casual, and dismissive. It lacks the required formality and respect.",
  "suggestions": [
    "Use formal language instead of casual expressions like 'yeah'",
    "Replace dismissive phrasing with committed language",
    "Show appreciation for feedback rather than calling it a 'complaint'"
  ]
}`),
			}, {
				Name:            "technical_accuracy_good",
				ReferenceAnswer: "HTTP 404 indicates that the requested resource was not found on the server.",
				ActualAnswer:    "A 404 error means the server cannot locate the requested resource.",
				Criterion:       "technical accuracy - response must correctly explain technical concepts",
				Evals: createEvals(1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "The actual response correctly explains the 404 error concept with accurate technical information and achieves the same effectiveness as the golden answer.",
  "suggestions": []
}`),
			}, {
				Name:            "technical_accuracy_poor",
				ReferenceAnswer: "HTTP 404 indicates that the requested resource was not found on the server.",
				ActualAnswer:    "404 means your internet connection is broken.",
				Criterion:       "technical accuracy - response must correctly explain technical concepts",
				Evals: createEvals(0.0, 0.2, `{
  "score": 0.0,
  "reasoning": "The actual response is technically incorrect. 404 errors are server-side issues, not client internet connection problems.",
  "suggestions": [
    "Correct the explanation: 404 is about missing resources, not connectivity",
    "Distinguish between client-side and server-side errors",
    "Explain that 404 means 'resource not found on server'"
  ]
}`),
			}, {
				Name:            "conciseness_good",
				ReferenceAnswer: "The meeting is at 3 PM.",
				ActualAnswer:    "Meeting: 3 PM",
				Criterion:       "conciseness - response should be brief while containing essential information",
				Evals: createEvals(0.8, 1.0, `{
  "score": 0.95,
  "reasoning": "The actual response is more concise while preserving all essential information.",
  "suggestions": []
}`),
			}, {
				Name:            "conciseness_poor",
				ReferenceAnswer: "The meeting is at 3 PM.",
				ActualAnswer:    "I would like to inform you that the upcoming meeting that we have scheduled is going to take place at the time of 3 PM in the afternoon.",
				Criterion:       "conciseness - response should be brief while containing essential information. Responses that are 3x+ longer than necessary with significant redundancy should score ≤0.4. Excessive verbosity degrades response quality and user experience.",
				Evals: createEvals(0.2, 0.4, `{
  "score": 0.3,
  "reasoning": "The actual response is unnecessarily verbose and contains redundant information (afternoon after PM, overly formal phrasing).",
  "suggestions": [
    "Remove redundant phrases like 'I would like to inform you'",
    "Eliminate unnecessary details like 'in the afternoon' after PM",
    "Use direct, simple language"
  ]
}`),
			}, {
				Name:            "relevance_on_topic",
				ReferenceAnswer: "The capital of France is Paris.",
				ActualAnswer:    "Paris is the capital city of France.",
				Criterion:       "relevance - response must directly address the question asked",
				Evals: createEvals(0.8, 1.0, `{
  "score": 1.0,
  "reasoning": "The actual response directly and completely addresses the question with relevant information.",
  "suggestions": []
}`),
			}, {
				Name:            "relevance_off_topic",
				ReferenceAnswer: "The capital of France is Paris.",
				ActualAnswer:    "France is known for its wine and cuisine.",
				Criterion:       "relevance - response must directly address the question asked",
				Evals: createEvals(0.0, 0.3, `{
  "score": 0.1,
  "reasoning": "The actual response is off-topic. While it mentions France, it doesn't answer the question about the capital city.",
  "suggestions": [
    "Answer the specific question asked about France's capital",
    "Stay focused on the requested information rather than general facts"
  ]
}`),
			}, {
				Name:            "clarity_clear",
				ReferenceAnswer: "To reset your password, click the 'Forgot Password' link on the login page.",
				ActualAnswer:    "Click 'Forgot Password' on the login page to reset your password.",
				Criterion:       "clarity - instructions should be easy to understand and follow",
				Evals: createEvals(1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "The actual response provides clear, easy-to-follow instructions with the same essential information and achieves equal effectiveness despite minor word order difference.",
  "suggestions": []
}`),
			}, {
				Name:            "clarity_confusing",
				ReferenceAnswer: "To reset your password, click the 'Forgot Password' link on the login page.",
				ActualAnswer:    "There's a thing you can use that might help with authentication credential restoration if you access the entry portal interface.",
				Criterion:       "clarity - instructions should be easy to understand and follow",
				Evals: createEvals(0.0, 0.3, `{
  "score": 0.1,
  "reasoning": "The actual response is confusing and uses unnecessarily complex language that obscures the simple instructions.",
  "suggestions": [
    "Use simple, direct language instead of technical jargon",
    "Provide specific action items rather than vague descriptions",
    "Be explicit about what to click and where to find it"
  ]
}`),
			}, {
				Name:            "completeness_thorough",
				ReferenceAnswer: "To bake a cake: mix flour, sugar, eggs, and butter; bake at 350°F for 30 minutes.",
				ActualAnswer:    "To bake a cake: combine 2 cups flour, 1 cup sugar, 2 eggs, and 1/2 cup butter; mix well; bake at 350°F for 30 minutes; cool before removing from pan.",
				Criterion:       "completeness - response should include all necessary steps and details",
				Evals: createEvals(1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "The actual response includes all required information plus helpful additional details like measurements and cooling instructions, exceeding completeness expectations.",
  "suggestions": []
}`),
			}, {
				Name:            "completeness_incomplete",
				ReferenceAnswer: "To bake a cake: mix flour, sugar, eggs, and butter; bake at 350°F for 30 minutes.",
				ActualAnswer:    "Mix some ingredients and bake.",
				Criterion:       "completeness - response should include all necessary steps and details",
				Evals: createEvals(0.0, 0.3, `{
  "score": 0.1,
  "reasoning": "The actual response is severely incomplete, missing specific ingredients, temperature, timing, and mixing instructions.",
  "suggestions": [
    "Specify the required ingredients: flour, sugar, eggs, butter",
    "Include baking temperature: 350°F",
    "Provide baking time: 30 minutes",
    "Add mixing instructions"
  ]
}`),
			}, {
				Name:            "consistency_aligned",
				ReferenceAnswer: "Our company values customer satisfaction above all else.",
				ActualAnswer:    "Customer satisfaction is our top priority and guiding principle.",
				Criterion:       "consistency - response should align with stated company values and messaging",
				Evals: createEvals(0.8, 1.0, `{
  "score": 0.95,
  "reasoning": "The actual response perfectly aligns with the company value, using similar language and maintaining the same priority focus.",
  "suggestions": []
}`),
			}, {
				Name:            "consistency_contradictory",
				ReferenceAnswer: "Our company values customer satisfaction above all else.",
				ActualAnswer:    "Profit margins are our primary concern, though we do consider customer needs occasionally.",
				Criterion:       "consistency - response should align with stated company values and messaging",
				Evals: createEvals(0.0, 0.2, `{
  "score": 0.0,
  "reasoning": "The actual response directly contradicts the stated company value by prioritizing profit over customer satisfaction.",
  "suggestions": [
    "Align messaging with stated company values",
    "Prioritize customer satisfaction in all communications",
    "Remove contradictory statements about profit being primary"
  ]
}`),
			}, {
				Name:            "empathy_understanding",
				ReferenceAnswer: "I understand this situation is frustrating. Let me help you resolve it.",
				ActualAnswer:    "I can see why this would be upsetting. I'm here to help you fix this problem.",
				Criterion:       "empathy - response should acknowledge customer emotions and show understanding",
				Evals: createEvals(1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "The actual response demonstrates empathy by acknowledging emotions and offering supportive assistance, achieving the same quality and effectiveness as the golden answer.",
  "suggestions": []
}`),
			}, {
				Name:            "empathy_dismissive",
				ReferenceAnswer: "I understand this situation is frustrating. Let me help you resolve it.",
				ActualAnswer:    "This is a simple issue. Just follow the standard procedure.",
				Criterion:       "empathy - response should acknowledge customer emotions and show understanding",
				Evals: createEvals(0.0, 0.3, `{
  "score": 0.1,
  "reasoning": "The actual response is dismissive and fails to acknowledge the customer's emotional state or show understanding.",
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
					// Run eachh test case in parallel (this may offend quotas)
					t.Parallel()
					t.Cleanup(wg.Done)
					// Create a child observer for this test case
					testObs := modelObs.Child(tc.Name)

					// Create context with combined evals tracer
					testCtx := context.Background()
					testCtx = agenttrace.WithTracer(testCtx, evals.BuildTracer(testObs, tc.Evals))

					// Call judge and verify response via evals callbacks
					_, err := judgeInstance.Judge(testCtx, &judge.Request{
						Mode:            judge.GoldenMode,
						ReferenceAnswer: tc.ReferenceAnswer,
						ActualAnswer:    tc.ActualAnswer,
						Criterion:       tc.Criterion,
					})
					if err != nil {
						t.Fatalf("Error running judge: %v", err)
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

// detectProjectID attempts to detect the GCP project ID from multiple sources
func detectProjectID(ctx context.Context, t *testing.T) string {
	// 1. Check environment variable first (highest priority)
	if projectID := os.Getenv("GOOGLE_CLOUD_PROJECT"); projectID != "" {
		t.Logf("Using project ID from GOOGLE_CLOUD_PROJECT env var: %s", projectID)
		return projectID
	}

	// 2. Check if running on GCE and use metadata service
	if metadata.OnGCE() {
		if projectID, err := metadata.ProjectIDWithContext(ctx); err == nil && projectID != "" {
			t.Logf("Using project ID from GCE metadata: %s", projectID)
			return projectID
		}
	}

	// 3. Try to get from Application Default Credentials
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err == nil && creds.ProjectID != "" {
		t.Logf("Using project ID from Application Default Credentials: %s", creds.ProjectID)
		return creds.ProjectID
	}

	// Note: We don't attempt to read from gcloud config as that would require
	// parsing ~/.config/gcloud/configurations/config_default and is considered
	// an anti-pattern. Users should set GOOGLE_CLOUD_PROJECT explicitly.

	// If we still don't have a project ID, skip the test
	t.Skip("Unable to detect Google Cloud project ID. Set GOOGLE_CLOUD_PROJECT environment variable or run on GCE.")
	return "" // unreachable
}
