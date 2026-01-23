/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package judge provides LLM-based evaluation of model outputs using structured rubrics.
//
// This package implements model-based grading as described in the Anthropic eval cookbook,
// allowing AI models to judge other AI model outputs using structured rubrics.
//
// # Overview
//
// The judge package provides:
//   - A common Interface for different LLM judge implementations
//   - Support for Claude (via Vertex AI) and Google Gemini models
//   - Single-criterion evaluation for clarity and simplicity
//   - Integration with the evals package for test harness usage
//
// # Usage
//
// Basic usage with the evals package:
//
//	// Create a judge instance (automatically selects implementation based on model name)
//	judgeInstance, err := judge.NewVertex(ctx, projectID, region, model)
//	if err != nil {
//		return err
//	}
//
//	// Create eval callbacks for different criteria
//	evals := map[string]evals.ObservableTraceCallback[*judge.Judgement]{
//	    "accuracy": judge.NewGoldenEval[*judge.Judgement](
//	        judgeInstance,
//	        "factual accuracy - response should be correct",
//	        goldenAnswer,
//	    ),
//	    "completeness": judge.NewGoldenEval[*judge.Judgement](
//	        judgeInstance,
//	        "completeness - response should address all aspects",
//	        goldenAnswer,
//	    ),
//	}
//
// # Scoring
//
// Judges score responses on a scale of 0.0 to 1.0, with 1.0 being perfect.
// Each criterion is evaluated independently, allowing for fine-grained
// analysis of different aspects of the response.
//
// # Integration
//
// The NewGoldenEval function creates an evals.ObservableTraceCallback that:
//  1. Extracts the response from trace.Result as formatted JSON
//  2. Sends it to the judge along with reference answer and criterion
//  3. Logs the judgment score, reasoning, and suggestions
//  4. Currently only logs results (threshold checking to be added)
//
// # Thread Safety
//
// All judge implementations (Claude and Google) are stateless and safe for concurrent use.
// The eval callback functions are also thread-safe and can be used concurrently.
package judge
