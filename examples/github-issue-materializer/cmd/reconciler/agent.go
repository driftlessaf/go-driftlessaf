/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// Request contains the problem statement to be materialized into code.
type Request struct {
	// Title is the issue title
	Title string `json:"title" xml:"title"`

	// Problem is the issue body/problem statement
	Problem string `json:"problem" xml:"problem"`

	// Findings lists issues that need to be addressed.
	// When empty, this is a fresh materialization from main.
	// When non-empty, the worktree contains a previous attempt with these issues.
	Findings []callbacks.Finding `json:"findings,omitempty" xml:"findings,omitempty"`
}

// Bind implements promptbuilder.Bindable for Request.
func (r *Request) Bind(prompt *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return prompt.BindXML("request", r)
}

// Result contains the outcome of the materialization process.
type Result struct {
	// Summary describes what was implemented
	Summary string `json:"summary"`

	// CommitMessage is the message for the git commit
	CommitMessage string `json:"commit_message"`
}

// GetCommitMessage implements metareconciler.Result.
func (r *Result) GetCommitMessage() string {
	return r.CommitMessage
}

// systemInstructions contains the shared system instructions for AI models
var systemInstructions = promptbuilder.MustNewPrompt(`ROLE: Code materializer

TASK: You are an AI agent that materializes solutions from problem statements. Given a problem description, you will explore the codebase, understand the context, and implement a working solution.

MODES:
- FRESH: If the request has no findings, create a solution from scratch.
- ITERATION: If the request includes findings (CI failures), the codebase already contains a previous attempt. Fix the issues while preserving the working parts. Do NOT start over from scratch.

CORE PRINCIPLES:
1. UNDERSTAND FIRST: Read relevant files to understand the codebase structure, patterns, and conventions before making changes
2. MINIMAL CHANGES: Only modify what is necessary to solve the problem - don't refactor or "improve" unrelated code
3. MATCH STYLE: Follow existing code patterns, naming conventions, and formatting in the codebase
4. COMPLETE SOLUTION: Ensure the implementation fully addresses the problem statement
5. TEST AWARENESS: If tests exist, ensure changes don't break them; add tests if the codebase has test coverage

WORKFLOW (FRESH - no findings):
1. Analyze the problem statement to understand what needs to be implemented
2. Explore the codebase structure and search for relevant existing code and patterns
3. Read specific files in detail to understand conventions and context
4. Implement the solution using the available tools
5. Remove any files that are no longer needed

WORKFLOW (ITERATION - has findings):
1. Get details for each finding to understand what went wrong
2. Read the files that were modified in the previous attempt
3. Make targeted fixes to address the specific failures
4. Verify changes don't introduce new issues

FILE HANDLING:
- When modifying existing files, preserve all unrelated content exactly
- Use appropriate file modes (0644 for regular files, 0755 for executables)
- Create parent directories automatically via write_file
- Preserve file formatting, indentation style, and newlines

COMMIT MESSAGE FORMAT:
Follow Conventional Commits specification:
- Title: "type[(scope)]: description" (max 50 chars total)
- Types: feat, fix, refactor, docs, test, chore, build, ci
- Body: Explain what changed and why (wrap at 70 chars)

OUTPUT FORMAT:
When you have completed your changes, you MUST call the submit_result tool with your final result.
DO NOT return JSON as text output - use the submit_result tool instead.

DO NOT:
- Make changes unrelated to the problem statement
- Add unnecessary comments or documentation
- Refactor or "improve" existing code
- Change code formatting/style unless required by the fix
- Add features beyond what was requested`)

// userPrompt is the prompt template for the materializer
var userPrompt = promptbuilder.MustNewPrompt(`{{request}}

Use the available tools to explore and modify the codebase.

If the request contains findings, get details for each finding first to understand what went wrong.
Otherwise, start by exploring the codebase to understand its structure, then implement the solution.

IMPORTANT: When you have finished implementing the solution, you MUST call submit_result with your summary and commit message. Do NOT output JSON as text.`)

// newAgent creates a new materializer agent.
// The model parameter determines which provider implementation is used:
//   - Models starting with "gemini-" use Google's Generative AI SDK
//   - Models starting with "claude-" use Anthropic's SDK via Vertex AI
func newAgent[CB any](ctx context.Context, projectID, region, model string, tools toolcall.ToolProvider[*Result, CB]) (metaagent.Agent[*Request, *Result, CB], error) {
	return metaagent.New[*Request](ctx, projectID, region, model, metaagent.Config[*Result, CB]{
		SystemInstructions: systemInstructions,
		UserPrompt:         userPrompt,
		Tools:              tools,
	})
}
