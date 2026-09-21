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
// Tool metadata comes from a `submitresult:"..."` struct tag on a blank field
// of the response type: comma-delimited key=value pairs (name, description,
// payload, payloadDescription, success). A comma inside a value is written
// escaped as `\,` (`\\,` inside a raw-string tag); an unescaped comma ends
// the value.
//
// Payload leniency: when the model JSON-encodes the payload object into a
// string instead of passing it as a nested object (a common model mistake),
// the handlers transparently decode the string and accept the submit instead
// of rejecting it with a parameter error. Strings that do not contain a JSON
// object are still rejected back to the model.
//
// A rejection records ErrParameter wrapping the cause — the same corrective
// hint the model receives, naming the parameter at fault — so the trace an
// engineer reads says which of the three causes fired (arguments that did not
// decode, an absent or mistyped parameter, or a stringified payload coercion
// declined) rather than collapsing them into one string. A declined
// stringified payload also records its length and a bounded, quoted opening
// prefix, which is what distinguishes a wrapped object from a YAML document
// from a truncated write. Consumers that gate on the class match ErrParameter
// with errors.Is, never the message.
package submitresult
