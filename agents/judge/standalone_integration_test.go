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

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/evals/report"
	"chainguard.dev/driftlessaf/agents/evals/testevals"
	"chainguard.dev/driftlessaf/agents/judge"
)

func TestStandalone(t *testing.T) {
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

	// createEvals creates all evaluation callbacks for standalone mode validation
	createEvals := func(metaJudgeInstance judge.Interface, minScore, maxScore float64, golden string) map[string]evals.ObservableTraceCallback[*judge.Judgement] {
		// Start with standard standalone evaluations
		evalMap := judge.Evals(judge.StandaloneMode)

		// Add range-specific evaluations using standard eval helpers
		maps.Copy(evalMap, map[string]evals.ObservableTraceCallback[*judge.Judgement]{
			"score-range": judge.ScoreRange(minScore, maxScore),
		})

		// Add judge-based meta-evaluations for standalone mode
		maps.Copy(evalMap, map[string]evals.ObservableTraceCallback[*judge.Judgement]{
			"judge-reasoning": judge.NewGoldenEval[*judge.Judgement](
				metaJudgeInstance,
				"standalone reasoning accuracy - Evaluate whether the 'reasoning' field correctly explains how well the response meets the criterion and justifies the score given. The reasoning should focus on criterion compliance, identifying strengths and weaknesses relative to the criterion requirements. Focus on whether the assessment logic is sound for standalone evaluation.",
				golden,
			),
			"judge-suggestions": judge.NewGoldenEval[*judge.Judgement](
				metaJudgeInstance,
				"standalone suggestion quality - Evaluate whether the 'suggestions' array provides useful improvements for better meeting the criterion. Suggestions should be specific, actionable, and directly target criterion compliance gaps. Empty suggestions are appropriate only for perfect scores (1.0).",
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
			t.Logf("Testing standalone judge with region=%s, model=%s", tt.region, tt.model)

			// Create namespaced observer for this model
			modelObs := obs.Child(tt.name)

			testCases := []struct {
				Name      string
				Response  string // Response to evaluate
				Criterion string
				Evals     map[string]evals.ObservableTraceCallback[*judge.Judgement]
			}{{
				Name: "perfect_clarity",
				Response: `I have sent you a password reset link via email:
1. Check your inbox for an email with the subject: "Chainguard password reset"
2. If you don't see it in your inbox, then check your spam folder
3. Open the reset link from the email
4. Create your new password when prompted

I will monitor this conversation for the next few minutes to provide troubleshooting assistance, if you should encounter any problems with these instructions.`,
				Criterion: "clarity - instructions should be easy to understand and follow",
				Evals: createEvals(metaJudgeInstance, 1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "Response perfectly meets the clarity criterion with clear, specific instructions that guide users through the complete process",
  "suggestions": []
}`),
			}, {
				Name:      "basic_completeness",
				Response:  "Python is a high-level, interpreted programming language known for its simplicity.",
				Criterion: "completeness - response should provide comprehensive information about the topic",
				Evals: createEvals(metaJudgeInstance, 0.2, 0.4, `{
  "score": 0.3,
  "reasoning": "Response provides basic information about Python but lacks comprehensive details about features, use cases, and capabilities for full completeness",
  "suggestions": [
    "Add information about Python's object-oriented capabilities",
    "Include details about Python's extensive library ecosystem",
    "Mention common use cases like web development, data science, automation"
  ]
}`),
			}, {
				Name:      "perfect_scientific_accuracy",
				Response:  "The Earth orbits around the Sun.",
				Criterion: "scientific accuracy - response must be scientifically correct",
				Evals: createEvals(metaJudgeInstance, 1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "Response perfectly meets the scientific accuracy criterion by stating a fundamental astronomical fact correctly",
  "suggestions": []
}`),
			}, {
				Name:      "poor_professional_tone",
				Response:  "Yeah, we got your complaint and we'll fix it when we get around to it.",
				Criterion: "professional tone - response should maintain formal, respectful language",
				Evals: createEvals(metaJudgeInstance, 0.0, 0.3, `{
  "score": 0.1,
  "reasoning": "Response completely fails the professional tone criterion with casual, dismissive language that shows no respect",
  "suggestions": [
    "Use formal language instead of casual expressions like 'yeah'",
    "Replace dismissive phrasing with committed language",
    "Show appreciation for feedback rather than calling it a 'complaint'"
  ]
}`),
			}, {
				Name:      "failing_relevance",
				Response:  "France is known for its wine and cuisine.",
				Criterion: "relevance - response must directly address the question about France's capital",
				Evals: createEvals(metaJudgeInstance, 0.0, 0.2, `{
  "score": 0.0,
  "reasoning": "Response completely fails the relevance criterion by discussing unrelated topics instead of addressing the capital question",
  "suggestions": [
    "Answer the specific question asked about France's capital",
    "State that Paris is the capital of France",
    "Stay focused on the requested information"
  ]
}`),
			}, {
				Name:      "excellent_technical_accuracy",
				Response:  "HTTP 404 indicates that the requested resource was not found on the server. The server was successfully contacted, but the specific resource does not exist.",
				Criterion: "technical accuracy - response must correctly explain technical concepts",
				Evals: createEvals(metaJudgeInstance, 0.9, 1.0, `{
  "score": 0.95,
  "reasoning": "Response demonstrates excellent technical accuracy with precise explanation of 404 errors and helpful clarification",
  "suggestions": []
}`),
			}, {
				Name:      "moderate_conciseness",
				Response:  "The upcoming meeting that we have scheduled for today will be taking place at 3 PM this afternoon.",
				Criterion: "conciseness - response should be brief while containing essential information",
				Evals: createEvals(metaJudgeInstance, 0.3, 0.6, `{
  "score": 0.4,
  "reasoning": "Response contains essential information but fails conciseness with redundant phrasing and unnecessary details",
  "suggestions": [
    "Remove redundant phrases like 'upcoming' and 'that we have scheduled'",
    "Eliminate unnecessary details like 'this afternoon' after PM",
    "Simplify to 'The meeting is at 3 PM today'"
  ]
}`),
			}, {
				Name: "perfect_empathy",
				Response: `I completely understand how frustrating and stressful this situation must be for you right now.
I can genuinely see why this would be so concerning and upsetting - anyone would feel the same way in your position.
I want to personally take ownership of this issue and ensure we resolve it for you immediately.
You shouldn't have to deal with this, and I'm here to make it right.
I'll stay with you through the entire resolution process to make sure you're completely satisfied.`,
				Criterion: "empathy - response should acknowledge customer emotions and show understanding",
				Evals: createEvals(metaJudgeInstance, 1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "Response perfectly meets the empathy criterion with specific emotional validation, personalized acknowledgment, and deep understanding",
  "suggestions": []
}`),
			}, {
				Name:      "good_structure",
				Response:  "Introduction: This document covers the API basics. Section 1: Authentication methods include API keys and OAuth. Section 2: Rate limits are 1000 requests per hour. Conclusion: Contact support for questions.",
				Criterion: "structure - response should be well-organized with clear sections",
				Evals: createEvals(metaJudgeInstance, 0.7, 0.9, `{
  "score": 0.8,
  "reasoning": "Response demonstrates good structure with clear sections and logical flow but could benefit from more detailed section headers",
  "suggestions": [
    "Use more descriptive section headers",
    "Add numbered subsections for complex topics"
  ]
}`),
			}, {
				Name:      "excellent_specificity",
				Response:  "The CPU temperature is currently 72°C, which is within the normal operating range of 65-80°C for this processor model under current load conditions.",
				Criterion: "specificity - response should provide concrete, measurable details",
				Evals: createEvals(metaJudgeInstance, 0.9, 1.0, `{
  "score": 0.95,
  "reasoning": "Response excellently meets specificity criterion with exact temperature, normal range, and contextual details",
  "suggestions": []
}`),
			}, {
				Name:      "poor_conciseness",
				Response:  "In my personal opinion, I believe that it would be advisable for you to consider the possibility of perhaps maybe thinking about potentially trying to restart your computer system if you are experiencing technical difficulties.",
				Criterion: "conciseness - response should be brief while containing essential information",
				Evals: createEvals(metaJudgeInstance, 0.0, 0.2, `{
  "score": 0.1,
  "reasoning": "Response completely fails conciseness with excessive hedging, redundant phrases, and unnecessary qualifiers",
  "suggestions": [
    "Remove hedging language like 'in my opinion', 'perhaps maybe'",
    "Simplify to direct instruction: 'Restart your computer'",
    "Eliminate redundant qualifiers and uncertainty phrases"
  ]
}`),
			}, {
				Name:      "adequate_helpfulness",
				Response:  "Try restarting your router. If that doesn't work, contact your ISP.",
				Criterion: "helpfulness - response should provide useful guidance for solving the problem",
				Evals: createEvals(metaJudgeInstance, 0.5, 0.7, `{
  "score": 0.6,
  "reasoning": "Response provides basic helpful guidance but lacks troubleshooting steps and specific instructions",
  "suggestions": [
    "Add step-by-step router restart instructions",
    "Include other troubleshooting options before contacting ISP",
    "Provide specific ISP contact guidance"
  ]
}`),
			}, {
				Name:      "perfect_factual_accuracy",
				Response:  "Water boils at 100°C (212°F) at sea level atmospheric pressure.",
				Criterion: "factual accuracy - response must state correct facts",
				Evals: createEvals(metaJudgeInstance, 1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "Response perfectly meets factual accuracy criterion with correct temperature and important atmospheric pressure context",
  "suggestions": []
}`),
			}, {
				Name:      "failing_factual_accuracy",
				Response:  "Water boils at 90°C under normal conditions.",
				Criterion: "factual accuracy - response must state correct facts",
				Evals: createEvals(metaJudgeInstance, 0.0, 0.2, `{
  "score": 0.0,
  "reasoning": "Response completely fails factual accuracy with incorrect boiling point temperature",
  "suggestions": [
    "Correct the boiling point to 100°C at sea level",
    "Specify atmospheric pressure conditions for accuracy"
  ]
}`),
			}, {
				Name:      "failing_relevance_with_tangential_mention",
				Response:  "Python is popular for data science and has many libraries. Some alternatives include R and MATLAB.",
				Criterion: "relevance - response must directly address the question about Python web development frameworks",
				Evals: createEvals(metaJudgeInstance, 0.0, 0.1, `{
  "score": 0.0,
  "reasoning": "Response completely fails to address web development frameworks, instead discussing data science and alternative languages",
  "suggestions": [
    "Focus specifically on web development frameworks like Django, Flask, FastAPI",
    "Remove unrelated information about data science and alternatives",
    "Directly answer the web development framework question"
  ]
}`),
			}, {
				Name:      "perfect_tone_consistency",
				Response:  "Welcome to our service! We're excited to help you get started. Please follow these steps to begin your journey with us.",
				Criterion: "tone consistency - response should maintain a welcoming, professional tone throughout",
				Evals: createEvals(metaJudgeInstance, 1.0, 1.0, `{
  "score": 1.0,
  "reasoning": "Response perfectly maintains consistent welcoming and professional tone throughout all sentences",
  "suggestions": []
}`),
			}, {
				Name:      "basic_instruction_completeness",
				Response:  "To install Docker: 1) Download from docker.com/get-started 2) Run the installer 3) Restart your computer 4) Open terminal and run 'docker --version' to verify 5) If version shows, installation succeeded",
				Criterion: "instruction completeness - response should include all necessary steps for the task",
				Evals: createEvals(metaJudgeInstance, 0.5, 0.7, `{
  "score": 0.6,
  "reasoning": "Response provides basic installation steps with verification but lacks platform-specific instructions, system requirements, and troubleshooting guidance",
  "suggestions": [
    "Add OS-specific installation instructions for Windows, Mac, Linux",
    "Include system requirements and prerequisites",
    "Add troubleshooting steps for common installation issues",
    "Include post-installation setup steps like starting Docker Desktop"
  ]
}`),
			}, {
				Name:      "poor_coherence",
				Response:  "The weather is nice. Database queries should use indexes. My favorite color is blue. SQL performance matters.",
				Criterion: "coherence - response should have logical flow and connected ideas",
				Evals: createEvals(metaJudgeInstance, 0.0, 0.3, `{
  "score": 0.1,
  "reasoning": "Response completely fails coherence with disconnected, random statements that have no logical relationship",
  "suggestions": [
    "Focus on a single topic or clearly connect different topics",
    "Remove unrelated personal information",
    "Create logical transitions between ideas"
  ]
}`),
			}, {
				Name:      "adequate_technical_depth",
				Response:  "HTTP status codes indicate server responses. 2xx means success, 4xx means client error, 5xx means server error.",
				Criterion: "technical depth - response should provide sufficient technical detail for understanding",
				Evals: createEvals(metaJudgeInstance, 0.5, 0.7, `{
  "score": 0.6,
  "reasoning": "Response provides basic technical categorization but lacks specific examples and deeper explanation of status code meanings",
  "suggestions": [
    "Include specific examples like 200, 404, 500",
    "Explain what each category means in practical terms",
    "Add common scenarios when each type occurs"
  ]
}`),
			}, {
				Name:      "failing_appropriateness",
				Response:  "Just wing it and see what happens. YOLO!",
				Criterion: "appropriateness - response should be suitable for a professional software development context",
				Evals: createEvals(metaJudgeInstance, 0.0, 0.2, `{
  "score": 0.0,
  "reasoning": "Response completely fails appropriateness with casual slang and unprofessional advice unsuitable for development context",
  "suggestions": [
    "Use professional language appropriate for software development",
    "Provide structured guidance instead of casual advice",
    "Remove slang expressions like 'YOLO'"
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
					testCtx = agenttrace.WithTracer(testCtx, evals.BuildTracer(testObs, tc.Evals))

					// Call judge with standalone mode and verify response via evals callbacks
					_, err := judgeInstance.Judge(testCtx, &judge.Request{
						Mode:         judge.StandaloneMode,
						ActualAnswer: tc.Response,
						Criterion:    tc.Criterion,
					})
					if err != nil {
						t.Fatalf("Error running standalone judge: %v", err)
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
