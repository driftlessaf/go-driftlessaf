/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package submitresult provides tool definitions for AI agents to submit their
// final results.
//
// It exposes ClaudeTool, GoogleTool, and OpenAITool constructors that build
// executor submit metadata for the terminal submit_result tool, which agents
// call to return a structured response at the end of a conversation. The
// handlers parse the call into a toolcall.SubmitOutcome; the executor decides
// whether the parsed response commits (ending the run) after running its
// registered result validators (see the executors' WithResultValidator
// option), or is rejected back to the model with the validators' findings so
// the loop continues.
package submitresult
