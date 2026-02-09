/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub PR validator reconciler with agentic fix capabilities.
//
// This example demonstrates the recommended pattern for building AI-powered
// reconcilers using the metaagent framework and toolcall.ToolProvider interface.
//
// # Architecture
//
// The reconciler validates PR titles and descriptions against conventional commit
// conventions. When the driftlessaf/autofix label is present and validation fails,
// it delegates to an AI agent to fix the issues automatically.
//
// The agent is configured via the AGENT_MODEL env var, supporting both Gemini and
// Claude models through Vertex AI:
//   - gemini-2.5-flash (default)
//   - claude-sonnet-4-5@20250929
//
// # Tool Pattern
//
// PR tools are implemented using the toolcall.ToolProvider interface with
// callback functions for testability:
//
//	prTools := NewPRTools(gh, owner, repo, prNumber)
//	result, err := agent.Execute(ctx, prContext, prTools)
//
// PRTools uses func fields (like WorktreeCallbacks) rather than embedding the
// GitHub client directly, enabling unit testing without a live API.
//
// # Files
//
//   - main.go: Reconciler entry point and PR validation orchestration
//   - agent.go: Metaagent construction (thin wrapper around metaagent.New)
//   - prtools.go: PRTools callbacks, ToolProvider implementation, and handler factories
//   - prompts.go: System and user prompt definitions
//   - types.go: PRContext (request) and PRFixResult (response) types
package main
