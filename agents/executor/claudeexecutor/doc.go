/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package claudeexecutor provides a generic executor for Claude-based agents that
// reduces boilerplate while maintaining flexibility for agent-specific logic.
//
// The executor handles the common conversation loop pattern including:
//   - Prompt rendering from templates
//   - Message streaming and accumulation
//   - Tool call execution and response handling
//   - JSON response parsing
//   - Trace management for evaluation
//
// # Basic Usage
//
// Create an executor with a Messages service and prompt template:
//
//	client := anthropic.NewClient(
//	    vertex.WithGoogleAuth(ctx, region, projectID, "https://www.googleapis.com/auth/cloud-platform"),
//	)
//
//	tmpl, _ := template.New("prompt").Parse("Analyze: {{.Input}}")
//
//	exec, err := claudeexecutor.NewWithMessages[*Request, *Response](
//	    client.Messages,
//	    tmpl,
//	    claudeexecutor.WithModel[*Request, *Response]("claude-3-opus@20240229"),
//	    claudeexecutor.WithMaxTokens[*Request, *Response](16000),
//	)
//	if err != nil {
//	    return nil, err
//	}
//
//	// Define tools if needed
//	tools := map[string]claudetool.Metadata[*Response]{
//	    "read_file": {
//	        Definition: anthropic.ToolParam{
//	            Name:        "read_file",
//	            Description: anthropic.String("Read a file"),
//	            InputSchema: anthropic.ToolInputSchemaParam{
//	                Properties: map[string]any{
//	                    "path": map[string]any{
//	                        "type": "string",
//	                        "description": "File path",
//	                    },
//	                },
//	                Required: []string{"path"},
//	            },
//	        },
//	        Handler: func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[*Response]) map[string]any {
//	            // Tool implementation
//	            return map[string]any{"content": "file contents"}
//	        },
//	    },
//	}
//
//	// Execute the agent
//	response, err := exec.Execute(ctx, request, tools)
//
// # Options
//
// The executor supports several configuration options:
//   - WithAttribution: Set canonical and compatibility route attribution
//   - WithModel: Override the default model (defaults to claude-sonnet-4-6)
//   - WithRoutedModel: Set separate provider-wire and logical capability model IDs
//   - WithMaxTokens: Set maximum response tokens (defaults to 8192, max 128000)
//   - WithTemperature: Set response temperature (defaults to 0.1)
//   - WithoutTemperature: Omit temperature when an explicit route disallows sampling parameters
//   - WithSystemInstructions: Provide system-level instructions
//   - WithThinking: Enable extended thinking mode with a token budget
//   - WithCacheFirstUserBlock: Also cache the first user message (off by default)
//   - WithCacheTTL: Set the cache breakpoint lifetime to 5 minutes (default) or 1 hour
//   - WithUserPromptSuffix: Render a static suffix as a second block of the
//     initial user message, outside the shared cacheable prefix (off by default)
//   - WithMaxToolCallsBeforeFinalize: Soft-cap the agentic loop (off by default)
//   - WithForceSubmitToolChoice: Force the terminal submit tool via tool_choice (off by default)
//   - WithTruncatedToolCallRetries: Bound the retries when the output-token cap
//     cuts off a tool call (defaults to 1)
//   - WithInputBudget: Keep every request inside the model's context window
//     (off by default; see Input Budget)
//
// # Prompt Caching
//
// Anthropic prompt caching is enabled by default (disable with
// WithoutCacheControl). The executor places cache breakpoints on the static
// request prefix — the last tool definition and the system prompt — and
// advances a moving breakpoint along the conversation tail each turn. The
// tail breakpoint means the accumulated message history (the rendered prompt
// and every prior tool result) is written to the cache once and read at the
// cached-token price on subsequent turns, instead of being re-billed at the
// full input price on every turn of the loop. Two tail markers are retained
// (current and previous) so a turn that appends many blocks — for example
// several parallel tool calls — still resumes from the prior turn's cache
// entry. Cache reads and writes are reported per turn via the
// gen_ai.usage.cache_* metrics and on the trace, along with the cache TTL.
//
// Breakpoints live for 5 minutes by default, measured from the start of each
// request, so a turn that generates for longer than that finds the cache
// expired and writes the whole prefix again. WithCacheTTL(time.Hour) keeps
// entries alive across such turns at a higher write price (2x the base input
// price instead of 1.25x); every marker the executor places carries it.
//
// WithUserPromptSuffix extends the shared prefix across executions: it splits
// the initial user message into a leading payload block (the rendered prompt,
// carrying a cache breakpoint) and a trailing static suffix block. Because
// cache entries are matched at breakpoint block boundaries, executions that
// share the payload but differ only in the suffix — for example multi-pass
// reviewers examining one changeset through different lenses — read the
// tools + system + payload prefix from a single cache entry instead of each
// re-paying the payload at full input price.
//
// # Input Budget
//
// A provider rejects a request whose input and max_tokens together exceed the
// model's context window, and resending the same conversation fails the same
// way. WithInputBudget(contextWindow) checks each request before the provider
// sees it. Without the option the executor makes no count_tokens call.
//
// A contextWindow of 0 takes the window from the provider-facts registry
// (model.Info.ContextWindow) for the logical model. A positive value
// overrides the registry. NewWithMessages fails when the window is unknown or
// the budget is 0 or less, whatever the order of the options.
//
// The budget is the window, less the request's max_tokens, less 1% of the
// window for drift between count_tokens and the serving count. For a
// 1,000,000-token window and 32,000 max_tokens it is 958,000 tokens.
//
// Before every provider call, after the cache tail advances, the executor
// checks the request in this order:
//
//  1. A local upper bound, the JSON bytes of the count request plus 4,096,
//     is at most the budget: send the request. A text token is never shorter
//     than a byte, so no count call is made. A request with an image or
//     document block, in a message or a tool result, has no local bound: the
//     provider tokenises the decoded media, so the request is always counted.
//  2. Otherwise Messages.CountTokens counts the request with the fields the
//     request sends: model, messages, system, tools, tool_choice and
//     thinking. A transient failure retries like a streamed turn and then
//     requeues. Any other failure returns an error and sends nothing.
//  3. A count over the budget reduces the oldest eligible older tool results
//     until the estimated saving covers the excess, then recounts. A count
//     still over the budget reduces every eligible result and recounts again.
//  4. A count still over the budget returns *InputTooLargeError and makes no
//     provider call.
//
// A turn over the budget makes three count calls at most.
//
// An older tool result is one before the last assistant message. A result
// that the model has not read yet is never reduced. A result is eligible
// when its content is text only, it is not an error (IsError, or a top-level
// "error" key in its JSON), it is at least 4,096 bytes, and it is not
// reduced already. Results of the submit tool and the suspend tool are never
// reduced, because they carry the validator findings and the human answer.
//
// Reduction replaces the result in place with a JSON stub. The stub keeps
// tool_use_id, so the tool_use block stays paired. Its fields are
// executor_reduced, tool, tool_use_id, original_bytes, the top-level scalar
// values of the original result (such as paging offsets), findings, the first
// and last 1,024 bytes as head and tail, and a note that tells the model to call the
// tool again. findings keeps every string at any depth beneath an error,
// errors, error_message, findings or summary key, because some results (such
// as a nested log analysis) cost a model call to produce again. Each string
// is cut to 512 bytes and each path to 256 bytes, and the findings JSON is
// at most 2,048 bytes. Reduction invalidates the prompt cache from the first
// reduced block, so the next request writes the cache again.
//
// InputTooLargeError unwraps to executor.ErrInputTooLarge, so a caller can
// match it without importing this package. Its text avoids the phrases that
// classify an error as a stream failure or a turn-limit stop. A turn that
// reduces or rejects logs one clog line and increments the pseudo-tool
// counter input_budget_reduction or input_budget_rejection.
//
// # Extended Thinking
//
// Extended thinking allows Claude to show its internal reasoning process before
// responding. When enabled, reasoning blocks are captured in the trace:
//
//	exec, err := claudeexecutor.NewWithMessages[*Request, *Response](
//	    client.Messages,
//	    prompt,
//	    claudeexecutor.WithThinking[*Request, *Response](2048), // 2048 token budget for thinking
//	)
//
// Reasoning blocks are stored in trace.Reasoning as []agenttrace.ReasoningContent,
// where each block contains:
//   - Thinking: the reasoning text
//
// Note: When thinking is enabled, temperature is automatically set to 1.0 as required
// by the Claude API. See: https://docs.claude.com/en/docs/build-with-claude/extended-thinking
//
// # Claude Opus 4.7 Compatibility
//
// Opus 4.7 introduced two breaking changes that the executor handles transparently
// so callers don't need model-aware logic:
//
//   - Sampling parameters (temperature, top_p, top_k) are rejected with a 400.
//     WithTemperature is silently dropped for Opus 4.7 models; a warning is logged
//     once per Execute if the caller explicitly set it.
//   - Extended-thinking budgets are replaced by adaptive thinking. WithThinking(N)
//     is mapped to adaptive thinking for Opus 4.7 (the budget is advisory to the
//     model via adaptive mode). A warning is logged once per Execute noting the
//     mapping. Display is set to "summarized" so reasoning traces remain populated.
//
// See: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7
//
// # Type Safety
//
// The executor is generic over Request and Response types, ensuring type safety
// throughout the conversation. The trace parameter in tool handlers is properly
// typed with the Response type.
package claudeexecutor
