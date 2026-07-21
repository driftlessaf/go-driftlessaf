/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package metaagent provides a generic framework for building AI agents.
//
// The framework is fully generic over three type parameters:
//   - Req: The request type (must implement promptbuilder.Bindable)
//   - Resp: The structured response type returned by the agent
//   - CB: The callbacks type providing tool implementations
//
// This design allows agents to be composed with any combination of tools
// from the toolcall package (worktree tools, finding tools, custom tools).
//
// # Model Support
//
// The model parameter determines which provider implementation is used:
//   - Models starting with "gemini-" use Google's Generative AI SDK (native)
//   - Models starting with "claude-" use Anthropic's SDK via Vertex AI (native)
//   - Models in "publisher/model" format use Vertex AI's OpenAI-compatible endpoint
//
// # Usage
//
// Define your callback type by composing tool callbacks:
//
//	type MyCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]
//
// Create the corresponding tool provider:
//
//	tools := toolcall.NewFindingToolsProvider[*Result, toolcall.WorktreeTools[toolcall.EmptyTools]](
//	    toolcall.NewWorktreeToolsProvider[*Result, toolcall.EmptyTools](
//	        toolcall.NewEmptyToolsProvider[*Result](),
//	    ),
//	)
//
// Configure and create the agent:
//
//	config := metaagent.Config[*Result, MyCallbacks]{
//	    SystemInstructions: systemPrompt,
//	    UserPrompt:         userPrompt,
//	    Tools:              tools,
//	}
//
//	agent, err := metaagent.New[*Request, *Result, MyCallbacks](ctx, projectID, region, model, config)
//	result, err := agent.Execute(ctx, request, callbacks)
//
// The agent uses the submit_result tool to return structured results. The Resp
// type's JSON tags define the schema for the tool's payload.
//
// # Suspend/Resume (ask-a-friend)
//
// Setting Config.SuspendToolName advertises a held-out ask-a-friend tool; when
// the model calls it, Execute returns a *checkpoint.Suspension instead of a
// Resp, and the paused conversation is later continued through the opt-in
// Resumer capability (obtained via AsResumer). Only the Claude backend
// supports this today — the Gemini and OpenAI-compatible backends reject a
// set SuspendToolName at construction until their executors grow suspend
// support. See the suspend package for the park/wake orchestration around it.
package metaagent
