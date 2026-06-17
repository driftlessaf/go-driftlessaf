/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package submitresult provides tool definitions for AI agents to submit their
// final results.
//
// It exposes ClaudeTool, GoogleTool, and OpenAITool constructors that build
// executor tool metadata for the terminal submit_result tool, which agents call
// to return a structured response at the end of a conversation.
//
// Each provider also offers a non-terminal validate_result companion
// (ClaudeValidateTool and friends) that advertises the identical schema and
// reports whether a payload would be accepted, without ending the run. The
// SubmitAndValidateForResponse constructors build the submit/validate pair from
// one Options so they share a schema and submit_result's payload errors point
// the model at validate_result.
package submitresult
