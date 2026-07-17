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
//
// Payload leniency: when the model JSON-encodes the payload object into a
// string instead of passing it as a nested object (a common model mistake),
// the handlers transparently decode the string and accept the submit instead
// of rejecting it with a parameter error. Strings that do not contain a JSON
// object are still rejected back to the model.
package submitresult
