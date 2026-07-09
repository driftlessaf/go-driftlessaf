/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/result"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"
)

// Interface is the public interface for Claude agent execution
type Interface[Request promptbuilder.Bindable, Response any] interface {
	// Execute runs the agent conversation with the given request and tools
	// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
	Execute(ctx context.Context, request Request, tools map[string]claudetool.Metadata[Response], seedToolCalls ...anthropic.ToolUseBlock) (Response, error)
}

// DefaultMaxTurns is the default maximum number of conversation turns (LLM
// round-trips) before the executor aborts. Each turn corresponds to one
// Claude API call. This prevents runaway loops when the model keeps calling
// tools without converging on a result.
const DefaultMaxTurns = 200

// DefaultToolCallConcurrency is the default bound on how many of a single
// turn's tool calls run concurrently. Models routinely emit several
// independent tool calls in one turn (parallel tool use); dispatching their
// handlers concurrently cuts wall-clock latency. Override with
// WithToolCallConcurrency — a value of 1 restores strictly sequential
// dispatch.
const DefaultToolCallConcurrency = 10

// executor provides the private implementation
type executor[Request promptbuilder.Bindable, Response any] struct {
	client               anthropic.Client
	modelName            string
	systemInstructions   *promptbuilder.Prompt
	prompt               *promptbuilder.Prompt
	maxTokens            int64
	maxTurns             int // maximum conversation turns before aborting
	temperature          float64
	temperatureSet       bool                          // true when WithTemperature was applied; lets us warn if it gets dropped for a model that doesn't accept sampling params
	thinkingBudgetTokens *int64                        // nil = disabled, non-nil = enabled with budget
	submitTool           claudetool.Metadata[Response] // opt-in: set via WithSubmitResultProvider
	genaiMetrics         *metrics.GenAI                // OpenTelemetry metrics for token usage and tool calls
	retryConfig          retry.RetryConfig             // retry configuration for transient Claude API errors
	resourceLabels       map[string]string             // resource labels for GCP billing attribution

	// cacheControl enables Anthropic prompt caching. When true, the executor places
	// cache breakpoints on tool definitions and the system prompt so the API can skip
	// re-processing them on subsequent turns and executions. Cached tokens are read at
	// 10% of the base input token price (5-min TTL, shared across all requests with
	// the same prefix within the same org).
	// Enabled by default — disable with WithoutCacheControl() if needed.
	// See: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
	cacheControl bool

	// cacheFirstUserBlock, when true, places an additional cache breakpoint on the
	// first user content block (the rendered prompt). This caches the initial user
	// message alongside the tool definitions and system prompt, so a large per-request
	// payload embedded in the prompt is read from cache on the second and subsequent
	// turns of a single conversation instead of re-billing at full price each turn.
	// Requires cacheControl. Off by default — opt in with WithCacheFirstUserBlock()
	// for agents whose first user message is large and whose loop spans several turns.
	// The breakpoint is only placed when doing so keeps the request within the API's
	// limit of four cache breakpoints.
	cacheFirstUserBlock bool

	// maxToolCallsBeforeFinalize, when greater than zero, bounds the agentic loop by
	// nudging the model to finish once it has made this many tool calls. After the
	// live tool-call count reaches the threshold, a single instruction is injected
	// asking the model to call its terminal (submit) tool now. Zero disables the
	// nudge entirely. This is a soft cap distinct from maxTurns: it steers the model
	// toward emitting a result rather than aborting the run. It is only meaningful
	// when a submit tool is configured (the terminal tool to call); without one it
	// has no effect.
	maxToolCallsBeforeFinalize int

	// forceSubmitToolChoice, when true, forces the model to call its terminal
	// submit tool via tool_choice rather than leaving the choice to the model.
	// This removes the wasted turn the executor would otherwise spend
	// reactively redirecting a model that answered with text instead of
	// calling the submit tool. Off by default; opt in with
	// WithForceSubmitToolChoice. Only meaningful when a submit tool is
	// configured. Incompatible with thinking (validated in New).
	forceSubmitToolChoice bool

	// forceSubmitDeferUntilTool, when non-empty, names a tool whose presence in
	// the run's tool set defers the forced tool_choice past the first turn. When
	// that tool is registered, the first turn stays at auto so the model can
	// call it (for example to fetch deferred evidence) before being forced to
	// finalize. Empty means force on the first turn unconditionally. Only
	// consulted when forceSubmitToolChoice is true.
	forceSubmitDeferUntilTool string

	// toolCallConcurrency bounds how many of a single turn's tool calls run
	// concurrently when the model emits more than one (parallel tool use).
	// Defaults to DefaultToolCallConcurrency. A value of 1 runs the turn's tool
	// calls strictly in order, one at a time. Concurrent dispatch is only safe
	// when the registered tool handlers are themselves safe for concurrent use
	// (they share the trace, which is safe). Set via WithToolCallConcurrency.
	toolCallConcurrency int
}

// maxCacheBreakpoints is the Anthropic API's hard limit on the number of
// cache_control breakpoints permitted in a single request. The executor
// places at most one breakpoint each on the last tool definition, the system
// block, and the first user block, so it never approaches the limit — the
// guard exists so a future caller-supplied breakpoint or an additional
// executor breakpoint cannot silently push a request over the limit and cause
// the API to reject it.
// See: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
const maxCacheBreakpoints = 4

// New creates a new Executor with minimal required configuration
func New[Request promptbuilder.Bindable, Response any](
	client anthropic.Client,
	prompt *promptbuilder.Prompt,
	opts ...Option[Request, Response],
) (Interface[Request, Response], error) {
	// Validate inputs
	if prompt == nil {
		return nil, errors.New("prompt cannot be nil")
	}

	// Create GenAI metrics for observability
	// Uses a unified meter across all executors, with model name as a dimension
	genaiMetrics := metrics.NewGenAI("chainguard.ai.agents")

	e := &executor[Request, Response]{
		client:              client,
		modelName:           "claude-sonnet-4@20250514", // Default to Sonnet 4
		prompt:              prompt,
		maxTokens:           8192,            // Default max tokens
		maxTurns:            DefaultMaxTurns, // Default max conversation turns
		temperature:         0.1,             // Default temperature for consistency
		genaiMetrics:        genaiMetrics,
		retryConfig:         retry.DefaultRetryConfig(), // Default retry config for rate limit handling
		cacheControl:        true,                       // Prompt caching on by default — see cacheControl field comment
		toolCallConcurrency: DefaultToolCallConcurrency, // Concurrent tool dispatch by default — see WithToolCallConcurrency
	}

	// Apply options
	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	// Forcing a submit tool_choice is incompatible with extended thinking: the
	// API rejects a forced tool_choice while thinking is active. Checked after
	// all options are applied so the conflict is caught regardless of the order
	// the two options were supplied in.
	if e.forceSubmitToolChoice && e.thinkingBudgetTokens != nil {
		return nil, errors.New("WithForceSubmitToolChoice is incompatible with WithThinking: the API rejects a forced tool_choice while extended thinking is active")
	}

	return e, nil
}

// Execute runs the agent conversation with the given request and tools
// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
func (e *executor[Request, Response]) Execute(
	ctx context.Context,
	request Request,
	tools map[string]claudetool.Metadata[Response],
	seedToolCalls ...anthropic.ToolUseBlock,
) (response Response, err error) {
	// Bind the request to the prompt
	boundPrompt, err := request.Bind(e.prompt)
	if err != nil {
		return response, fmt.Errorf("failed to bind request to prompt: %w", err)
	}

	// Build the prompt string
	prompt, err := boundPrompt.Build()
	if err != nil {
		return response, fmt.Errorf("failed to build prompt: %w", err)
	}

	// Start trace — done completes and records via the context tracer
	trace, done := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() {
		done(response, err)
	}()

	clog.InfoContext(ctx, "Starting Claude agent execution",
		"prompt_length", len(prompt),
	)

	// Merge submit_result tool if configured (opt-in via WithSubmitResultProvider)
	if e.submitTool.Handler != nil {
		mergedTools := make(map[string]claudetool.Metadata[Response], len(tools)+1)
		maps.Copy(mergedTools, tools)

		name := e.submitTool.Definition.Name
		if name == "" {
			name = "submit_result"
		}
		if _, exists := mergedTools[name]; !exists {
			mergedTools[name] = e.submitTool
		}
		tools = mergedTools
	}

	// Assemble the base request parameters: sorted tool definitions, the
	// system prompt, the initial user message, and the cache breakpoints. The
	// temperature warning is deferred to the caller because it needs ctx.
	params, dropTemperatureWarn, err := e.assembleParams(prompt, tools)
	if err != nil {
		return response, err
	}
	if dropTemperatureWarn {
		clog.WarnContext(ctx, "dropping temperature: not supported by this model",
			"model", e.modelName, "temperature", e.temperature)
	}

	// Add thinking configuration if enabled. Opus 4.7 removed extended-thinking
	// budgets; adaptive is the only thinking-on mode. Map WithThinking to adaptive
	// for those models and warn that the requested budget was ignored. Display is
	// set to summarized so trace.Reasoning stays populated (Opus 4.7 omits thinking
	// content by default).
	// See: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7#extended-thinking-budgets-removed
	if e.thinkingBudgetTokens != nil {
		if supportsExtendedThinkingBudget(e.modelName) {
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfEnabled: &anthropic.ThinkingConfigEnabledParam{
					BudgetTokens: *e.thinkingBudgetTokens,
				},
			}
		} else {
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
					Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
				},
			}
			clog.WarnContext(ctx, "mapping WithThinking to adaptive thinking: extended-thinking budgets not supported by this model",
				"model", e.modelName, "budget_tokens", *e.thinkingBudgetTokens)
		}
	}

	// finalResult stores the result if a tool sets it
	var finalResult Response
	finalResultPtr := &finalResult

	// submitToolName is the configured terminal tool the model calls to
	// return its result. Empty when no submit tool is registered.
	submitToolName := e.submitToolName()

	// liveToolCalls counts the model's investigative (non-terminal) tool
	// calls across turns. It drives the optional early-finalize nudge
	// (maxToolCallsBeforeFinalize). The terminal submit tool is excluded so
	// the nudge measures investigation effort, not the act of finishing.
	liveToolCalls := 0
	// finalizeNudged records whether the early-finalize instruction has
	// already been injected, so it fires at most once per execution.
	finalizeNudged := false
	// deferredGateResolved records whether the deferral gate tool (when one is
	// configured via WithForceSubmitToolChoice) has been called at least once.
	// While a gate tool is registered but unresolved, the first turn was left
	// at auto by assembleParams; once it resolves, the submit tool is forced on
	// the next turn. False when no gate tool applies.
	deferredGateResolved := false

	// executeToolCall handles executing a single tool call and returning the
	// result. The handler writes any terminal result into resultPtr; the
	// sequential path passes the shared finalResultPtr, while the concurrent
	// path passes a per-call slot so handlers never race on the same pointer.
	executeToolCall := func(toolUse anthropic.ToolUseBlock, resultPtr *Response) (anthropic.ContentBlockParamUnion, error) {
		kvs := []any{"tool", toolUse.Name, "id", toolUse.ID}
		var args map[string]any
		if err := json.Unmarshal(normalizeToolUseInput(toolUse.Input), &args); err == nil {
			for k, v := range args {
				kvs = append(kvs, "args."+k, v)
			}
		}
		clog.InfoContext(ctx, "Executing tool call", kvs...)

		var result map[string]any

		if meta, ok := tools[toolUse.Name]; ok {
			// Execute registered handler with result pointer
			result = meta.Handler(ctx, toolUse, trace, resultPtr)
			// The model supplies a universal `reasoning` argument on every
			// call (auto-injected into each tool schema), but handlers record
			// curated param maps that drop it. Merge it back onto the
			// recorded call so the per-action rationale survives into the
			// trace and BigQuery.
			if r, ok := args["reasoning"].(string); ok {
				trace.AttachToolCallReasoning(toolUse.ID, r)
			}
		} else {
			// Unknown tool
			clog.ErrorContext(ctx, "Unknown tool requested", "tool", toolUse.Name)
			trace.BadToolCall(toolUse.ID, toolUse.Name,
				map[string]any{"input": toolUse.Input},
				fmt.Errorf("unknown tool: %q", toolUse.Name))

			result = map[string]any{
				"error": fmt.Sprintf("unknown tool: %q", toolUse.Name),
			}
		}

		// Marshal result
		resultBytes, err := json.Marshal(result)
		if err != nil {
			return anthropic.ContentBlockParamUnion{}, fmt.Errorf("failed to marshal tool result: %w", err)
		}

		return anthropic.ContentBlockParamUnion{
			OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: toolUse.ID,
				Content: []anthropic.ToolResultBlockParamContentUnion{{
					OfText: &anthropic.TextBlockParam{
						Text: string(resultBytes),
					},
				}},
			},
		}, nil
	}

	// Pre-execute seed tool calls and add them to messages
	for _, toolCall := range seedToolCalls {
		// Add assistant message with this tool call
		params.Messages = append(params.Messages, anthropic.MessageParam{
			Role: anthropic.MessageParamRoleAssistant,
			Content: []anthropic.ContentBlockParamUnion{{
				OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    toolCall.ID,
					Name:  toolCall.Name,
					Input: toolCall.Input,
				},
			}},
		})

		// Execute the tool call.
		result, err := executeToolCall(toolCall, finalResultPtr)
		if err != nil {
			return response, err
		}

		// Check if a tool set the final result during seed execution
		if !reflect.ValueOf(finalResult).IsZero() {
			clog.InfoContext(ctx, "Seed tool set final result, exiting immediately")
			e.recordTurns(ctx, 0, false)
			return finalResult, nil
		}

		// Add tool result to conversation
		params.Messages = append(params.Messages, anthropic.MessageParam{
			Role:    anthropic.MessageParamRoleUser,
			Content: []anthropic.ContentBlockParamUnion{result},
		})
	}

	// The named err return is load-bearing: the deferred Fail call below reads
	// it at function exit. Every error path must use `return ..., err` (or set
	// the named err before bare-returning) — a bare return inside a nested
	// block where err is shadowed via `:=` would silently bypass Fail.
	executeTurn := func(turn int) (_ Response, _ bool, err error) {
		llmTurn := trace.BeginTurn(turn, agenttrace.SystemAnthropic, e.modelName)
		defer func() {
			if err != nil {
				llmTurn.Fail(err)
			}
			llmTurn.End()
		}()

		// Per-turn retry config wires transient API errors that the retry
		// recovers from into the turn's Errors list. Without this, retries
		// that eventually succeed leave no trace of the transients in BQ.
		// Also count every retried attempt in genai.api.requests; the final
		// attempt is counted after RetryWithBackoff returns below.
		turnCfg := e.retryConfig
		turnCfg.OnAttemptError = llmTurn.RecordError
		turnCfg = e.withAPIRequestCounter(ctx, turnCfg)

		// Capture the cumulative prompt as sent to Anthropic. params.Messages
		// grows across turns (tool results are appended in-place after each
		// turn), so this row carries the full context the model saw. Gated
		// inside RecordRequest on agenttrace.WithPayloadsEnabled — a no-op
		// when payloads are disabled. See span_test.go for shape coverage.
		if err := llmTurn.RecordRequest(params.Messages); err != nil {
			clog.WarnContext(ctx, "failed to record llm prompt payload", "error", err)
		}

		// Stream response with retry for transient errors
		message, err := retry.RetryWithBackoff(ctx, turnCfg, "stream_message", isRetryableClaudeError, func() (anthropic.Message, error) {
			stream := e.client.Messages.NewStreaming(ctx, params)
			var msg anthropic.Message
			for stream.Next() {
				event := stream.Current()
				if err := msg.Accumulate(event); err != nil {
					// The SDK re-marshals accumulated content on content_block_stop
					// and message_stop to refresh its JSON cache. A tool_use block
					// whose Input is a non-nil zero-length json.RawMessage (model
					// emitted a tool call with no input_json_delta events) causes
					// json.Marshal to fail with "unexpected end of JSON input".
					// The structural accumulation is intact -- only the JSON cache
					// refresh failed -- so normalize the empty tool inputs to "{}"
					// and continue.
					if isEmptyRawMessageMarshalErr(err) && normalizeEmptyToolInputs(&msg) {
						continue
					}
					return msg, fmt.Errorf("failed to accumulate event: %w", err)
				}
			}
			if err := stream.Err(); err != nil {
				return msg, err
			}
			return msg, nil
		})
		e.recordAPIRequest(ctx, err)
		if err != nil {
			// If the error is a retryable Claude API error (429, 503, 504, 529) that
			// exhausted inner retries, signal the workqueue to back off instead of
			// immediately retrying — avoids contributing to API overload.
			if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableClaudeError, "Claude API"); requeueErr != nil {
				return response, true, requeueErr
			}
			return response, true, fmt.Errorf("failed to stream Claude response: %w", err)
		}

		// Record token usage in metrics and on the per-turn span. Trace-level
		// token totals are derived from turns[] in downstream consumers — see
		// agent_trace_costs.sql.
		if message.Usage.InputTokens > 0 || message.Usage.OutputTokens > 0 {
			e.recordTokenMetrics(ctx, message.Usage.InputTokens, message.Usage.OutputTokens)
			llmTurn.RecordTokens(message.Usage.InputTokens, message.Usage.OutputTokens)
		}

		// Capture the assistant content blocks (text + tool_use + thinking)
		// as the completion for this turn. Pairs with RecordRequest above to
		// produce a per-span row keyed on prompt_hash. Gated inside the call.
		if err := llmTurn.RecordResponse(message.Content); err != nil {
			clog.WarnContext(ctx, "failed to record llm response payload", "error", err)
		}

		// Record prompt cache metrics. The API response includes two cache-specific
		// token counts alongside the regular input/output tokens:
		//   - cache_read_input_tokens:     tokens served from cache (cheap, 0.1x price)
		//   - cache_creation_input_tokens: tokens written to cache (1.25x price, amortized over reads)
		// These are recorded as OTel counters and on the per-turn span so the
		// cost view can apply per-call cache pricing accurately.
		if e.cacheControl {
			cacheRead := message.Usage.CacheReadInputTokens
			cacheCreation := message.Usage.CacheCreationInputTokens
			if cacheRead > 0 || cacheCreation > 0 {
				e.recordCacheMetrics(ctx, cacheRead, cacheCreation)
				llmTurn.RecordCacheTokens(cacheRead, cacheCreation)
				clog.DebugContext(ctx, "Prompt cache metrics",
					"cache_read_tokens", cacheRead,
					"cache_creation_tokens", cacheCreation)
			}
		}

		// Strip degenerate empty text blocks (streamed with zero text_delta
		// events during provider anomaly windows) before the response is
		// consumed. Left in place, message.ToParam() would replay the empty
		// block on the next request — at either the tool-call append or the
		// text-redirect append below — and the API rejects it with a
		// non-retryable 400 ("messages: text content blocks must be
		// non-empty") that kills the conversation on its final turn. Applied
		// after RecordResponse so the trace payload preserves the raw anomaly.
		// A response stripped to zero content falls through to the "no
		// content" error below and is never appended to params.Messages.
		if normalizeEmptyTextBlocks(&message) {
			clog.WarnContext(ctx, "Stripped empty text block(s) from Claude response before replay")
		}

		// Process response
		var toolUseBlocks []anthropic.ToolUseBlock
		var textContent string

		for _, content := range message.Content {
			switch content.Type {
			case "text":
				textContent = content.Text
			case "tool_use":
				toolUseBlocks = append(toolUseBlocks, anthropic.ToolUseBlock{
					ID:    content.ID,
					Name:  content.Name,
					Input: normalizeToolUseInput(content.Input),
				})
			case "thinking", "redacted_thinking":
				// Gated on the WithPayloadsEnabled opt-in inside AppendReasoning:
				// raw thinking is confidential completion content, not structural
				// metadata, so it is only captured when payloads are enabled.
				trace.AppendReasoning(agenttrace.ReasoningContent{
					Thinking: content.Thinking,
				})
			}
		}

		// Handle tool calls
		if len(toolUseBlocks) > 0 {
			// Clear any forced ToolChoice from a prior text-fallback redirect; otherwise a failing forced call would lock every subsequent turn into the same tool.
			params.ToolChoice = anthropic.ToolChoiceUnionParam{}

			// Add Claude's response to conversation
			params.Messages = append(params.Messages, message.ToParam())

			// account records the per-tool bookkeeping that depends only on the
			// tool name: the tool-call metric, the investigative-call counter
			// that drives the early-finalize nudge (the terminal submit tool is
			// the model converging, not investigating, so it is excluded), and
			// resolution of the deferral gate tool. It runs sequentially before
			// dispatch so these shared counters are never raced by concurrent
			// handlers.
			for _, toolUse := range toolUseBlocks {
				e.recordToolCall(ctx, toolUse.Name)
				if submitToolName == "" || toolUse.Name != submitToolName {
					liveToolCalls++
				}
				if e.forceSubmitToolChoice && e.forceSubmitDeferUntilTool != "" &&
					toolUse.Name == e.forceSubmitDeferUntilTool {
					deferredGateResolved = true
				}
			}

			// Dispatch the turn's tool calls under a bounded pool. The model may
			// emit several independent tool calls in a single turn (parallel tool
			// use, which the Anthropic API documents as unordered); a concurrency
			// of 1 runs them strictly in order, higher values run them
			// concurrently. Each handler writes into its own result slot so the
			// shared finalResultPtr is never raced; results are then consumed in
			// the model's original order to preserve the tool_use/tool_result
			// pairing, and the first terminal result (in order) ends the run. Tool
			// handlers must be safe for concurrent use when concurrency exceeds 1.
			type toolOutcome struct {
				result anthropic.ContentBlockParamUnion
				err    error
			}
			outcomes := make([]toolOutcome, len(toolUseBlocks))
			perCallResults := make([]Response, len(toolUseBlocks))

			g := new(errgroup.Group)
			g.SetLimit(max(1, e.toolCallConcurrency))
			for i, toolUse := range toolUseBlocks {
				g.Go(func() error {
					res, cerr := executeToolCall(toolUse, &perCallResults[i])
					outcomes[i] = toolOutcome{result: res, err: cerr}
					return nil
				})
			}
			_ = g.Wait()

			var toolResults []anthropic.ContentBlockParamUnion
			for i := range toolUseBlocks {
				if outcomes[i].err != nil {
					return response, true, outcomes[i].err
				}
				toolResults = append(toolResults, outcomes[i].result)
				if !reflect.ValueOf(perCallResults[i]).IsZero() {
					clog.InfoContext(ctx, "Tool set final result, exiting conversation loop", "turns_completed", turn+1)
					e.recordTurns(ctx, turn+1, false)
					return perCallResults[i], true, nil
				}
			}

			// Add tool results to conversation
			params.Messages = append(params.Messages, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: toolResults,
			})

			// Optional early-finalize nudge. When the investigative tool-call
			// count reaches the configured threshold and a terminal tool is
			// configured, inject a single instruction asking the model to emit
			// its result now and force that tool on the next turn. This bounds
			// runaway investigation by steering toward a result rather than
			// aborting at maxTurns. Off when maxToolCallsBeforeFinalize is zero.
			if shouldNudgeFinalize(e.maxToolCallsBeforeFinalize, liveToolCalls, finalizeNudged, submitToolName) {
				finalizeNudged = true
				e.recordToolCall(ctx, "early_finalize_nudge")
				clog.InfoContext(ctx, "early-finalize threshold reached, nudging model to emit result",
					"live_tool_calls", liveToolCalls,
					"threshold", e.maxToolCallsBeforeFinalize)
				params.Messages = append(params.Messages, anthropic.MessageParam{
					Role: anthropic.MessageParamRoleUser,
					Content: []anthropic.ContentBlockParamUnion{
						anthropic.NewTextBlock(fmt.Sprintf("You have gathered enough evidence. Call the %s tool now to return your result based on what you have found so far.", submitToolName)),
					},
				})
				params.ToolChoice = anthropic.ToolChoiceParamOfTool(submitToolName)
			}

			// Force the submit tool on the next turn once the deferral gate tool
			// has resolved. assembleParams left the first turn at auto because
			// the gate tool was registered; now that it has been called, the
			// deferred evidence is available and the model should finalize.
			// Skipped when the early-finalize nudge above already forced the
			// submit tool, when no submit tool is configured, or when a forced
			// choice from a prior turn is already in place.
			if submitToolName != "" && deferredGateResolved &&
				params.ToolChoice.OfTool == nil {
				params.ToolChoice = anthropic.ToolChoiceParamOfTool(submitToolName)
			}

			return response, false, nil
		}

		// When submit_result is configured, it is the only valid exit path.
		// If Claude responds with text instead of calling submit_result,
		// force it to call the tool on the next turn using tool_choice.
		if e.submitTool.Handler != nil && textContent != "" {
			clog.WarnContext(ctx, "Claude responded with text instead of calling submit_result, redirecting with tool_choice")
			e.recordToolCall(ctx, "submit_result_redirect")

			params.Messages = append(params.Messages, message.ToParam())
			params.Messages = append(params.Messages, anthropic.MessageParam{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					anthropic.NewTextBlock("You must call the submit_result tool to return your response. Do not respond with plain text. If you encountered an error or cannot complete the task, call submit_result with an appropriate error or summary."),
				},
			})
			// Force Claude to call the submit_result tool on the next turn.
			params.ToolChoice = anthropic.ToolChoiceParamOfTool(submitToolName)
			return response, false, nil
		}

		// Fallback: parse text response as JSON when submit_result is not configured
		if textContent != "" {
			resp, err := result.Extract[Response](textContent)
			if err != nil {
				clog.ErrorContext(ctx, "Failed to parse Claude response",
					"response", textContent,
					"error", err)
				return response, true, fmt.Errorf("failed to parse response: %w", err)
			}

			clog.InfoContext(ctx, "Successfully completed Claude agent execution", "turns_completed", turn+1)
			e.recordTurns(ctx, turn+1, false)
			return resp, true, nil
		}

		return response, true, errors.New("no content in Claude's response")
	}

	// Conversation loop with bounded turns to prevent runaway executions.
	for turn := range e.maxTurns {
		resp, done, err := executeTurn(turn)
		// done=true on all terminal paths (including errors); || err != nil is a
		// safety net in case a future path sets err without setting done.
		if done || err != nil {
			return resp, err
		}
	}

	clog.ErrorContext(ctx, "Agent exceeded maximum conversation turns", "max_turns", e.maxTurns)
	e.recordTurns(ctx, e.maxTurns, true)
	return response, fmt.Errorf("agent exceeded maximum conversation turns (%d)", e.maxTurns)
}

// assembleParams builds the base request parameters shared by every turn: the
// sorted tool definitions, the system prompt, the initial user message, the
// sampling parameters, and the cache breakpoints. It returns the assembled
// params and a flag indicating that an explicitly-set temperature was dropped
// for a model that does not accept sampling params, so the caller can log the
// warning with its context. Pulling this out of Execute keeps the breakpoint
// placement and the breakpoint-count guard in one place that tests can drive
// directly without a live client.
func (e *executor[Request, Response]) assembleParams(prompt string, tools map[string]claudetool.Metadata[Response]) (anthropic.MessageNewParams, bool, error) {
	// Build tool definitions for Claude, sorted by name for deterministic ordering.
	//
	// Why sort? The Anthropic API uses prompt caching to avoid re-processing the
	// same content on every turn. It works by hashing the request prefix (tools →
	// system prompt → messages, in that order). If the hash matches a previous
	// request, cached tokens are served at 10% of the normal input token price.
	//
	// Go maps iterate in non-deterministic order, so without sorting, the tool
	// definitions would serialize differently on every turn — producing a different
	// hash and invalidating the cache every time, even though the tools haven't
	// changed. Sorting by name ensures a stable hash across turns and executions.
	toolDefs := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, meta := range tools {
		toolDefs = append(toolDefs, anthropic.ToolUnionParam{
			OfTool: &meta.Definition,
		})
	}
	slices.SortFunc(toolDefs, func(a, b anthropic.ToolUnionParam) int {
		return cmp.Compare(a.OfTool.Name, b.OfTool.Name)
	})

	// breakpoints counts the cache_control markers placed so far so the
	// running total stays within maxCacheBreakpoints. The first-user-block
	// breakpoint is only placed when room remains.
	breakpoints := 0

	// Place a cache breakpoint on the last tool definition.
	//
	// A "breakpoint" (cache_control) tells the API: "everything from the start of
	// the request up to and including this block can be cached." The API hashes
	// that prefix — if the next request has the same hash, the cached computation
	// is reused instead of re-processing all those tokens.
	//
	// Tools come first in the API prefix order (tools → system → messages), so
	// a breakpoint here caches all tool definitions. This benefits both multi-turn
	// conversations (same tools every turn) and separate executions that share the
	// same tool set (cache is keyed by content hash, not by session).
	if e.cacheControl && len(toolDefs) > 0 {
		toolDefs[len(toolDefs)-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
		breakpoints++
	}

	// Create initial messages, starting with the user prompt
	messages := []anthropic.MessageParam{{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewTextBlock(prompt),
		},
	}}

	// Create request parameters
	params := anthropic.MessageNewParams{
		Model:     e.modelName,
		MaxTokens: e.maxTokens,
		Messages:  messages,
		Tools:     toolDefs,
	}

	// Opus 4.7 removed the sampling-param fields (temperature, top_p, top_k);
	// the API returns a 400 when any are set to a non-default value. Gate here
	// so callers don't need model-aware logic.
	// See: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7#sampling-parameters-removed
	dropTemperatureWarn := false
	if supportsSamplingParams(e.modelName) {
		params.Temperature = anthropic.Float(e.temperature)
		// Extended thinking requires temperature=1.0.
		// See: https://docs.claude.com/en/docs/build-with-claude/extended-thinking#important-considerations-when-using-extended-thinking
		if e.thinkingBudgetTokens != nil {
			params.Temperature = anthropic.Float(1.0)
		}
	} else if e.temperatureSet {
		dropTemperatureWarn = true
	}

	// Add system instructions if provided
	if e.systemInstructions != nil {
		systemPrompt, err := e.systemInstructions.Build()
		if err != nil {
			return anthropic.MessageNewParams{}, false, fmt.Errorf("building system prompt: %w", err)
		}
		systemBlock := anthropic.TextBlockParam{Text: systemPrompt}
		// Place a second cache breakpoint on the system prompt. Since the API prefix
		// order is tools → system → messages, this breakpoint caches both the tool
		// definitions AND the system prompt together. On subsequent turns, the API
		// reads both from cache instead of re-processing them as fresh input tokens.
		if e.cacheControl {
			systemBlock.CacheControl = anthropic.NewCacheControlEphemeralParam()
			breakpoints++
		}
		params.System = []anthropic.TextBlockParam{systemBlock}
	}

	// Optionally place a cache breakpoint on the first user content block (the
	// rendered prompt). Messages come last in the API prefix order, so this
	// caches everything before it — the tool definitions, the system prompt,
	// and the initial user message — together. For agents whose first user
	// message carries a large payload and whose loop spans several turns, this
	// lets the API read that payload from cache on turns after the first
	// instead of re-billing it at full price. Off by default; only applied when
	// a breakpoint slot remains, so it can never push a request past the API
	// limit. The breakpoint lives on the per-block ContentBlockParamUnion, so
	// it travels with the message as params.Messages grows across turns.
	//
	// This is the last breakpoint the executor places, so the running count is
	// only read here; it is not incremented afterward.
	if e.cacheControl && e.cacheFirstUserBlock && breakpoints < maxCacheBreakpoints &&
		len(params.Messages) > 0 && len(params.Messages[0].Content) > 0 {
		if tb := params.Messages[0].Content[0].OfText; tb != nil {
			tb.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}

	// Optionally force the terminal submit tool on the first turn. This is the
	// last field set so it overrides the default (unset = auto). When a deferral
	// gate tool is registered the first turn stays at auto so the model can call
	// that tool first; the force is then applied on a later turn by the turn
	// loop. See shouldForceSubmitOnFirstTurn for the exact condition.
	if name := e.submitToolName(); name != "" && e.shouldForceSubmitOnFirstTurn(tools) {
		params.ToolChoice = anthropic.ToolChoiceParamOfTool(name)
	}

	return params, dropTemperatureWarn, nil
}

// submitToolName returns the configured terminal submit tool's name, or "" when
// no submit tool is registered. The default name "submit_result" is used when a
// submit tool is registered without an explicit name.
func (e *executor[Request, Response]) submitToolName() string {
	if e.submitTool.Handler == nil {
		return ""
	}
	if e.submitTool.Definition.Name == "" {
		return "submit_result"
	}
	return e.submitTool.Definition.Name
}

// shouldForceSubmitOnFirstTurn reports whether the forced submit tool_choice
// should be applied on the first turn. It is true only when the option is
// enabled and either no deferral gate tool is named or the named gate tool is
// not registered for this run. When the gate tool IS registered, the first turn
// stays at auto so the model can call it before being forced to finalize.
func (e *executor[Request, Response]) shouldForceSubmitOnFirstTurn(tools map[string]claudetool.Metadata[Response]) bool {
	if !e.forceSubmitToolChoice {
		return false
	}
	if e.forceSubmitDeferUntilTool == "" {
		return true
	}
	_, gateRegistered := tools[e.forceSubmitDeferUntilTool]
	return !gateRegistered
}

// shouldNudgeFinalize reports whether the early-finalize instruction should be
// injected on this turn. It fires once the investigative tool-call count
// reaches the threshold, and only when:
//   - a positive threshold is configured (zero disables the nudge),
//   - the nudge has not already been sent this execution, and
//   - a terminal submit tool is configured to steer the model toward.
//
// A non-positive threshold or an empty submit tool name always returns false,
// preserving identical behavior for callers that do not opt in.
func shouldNudgeFinalize(threshold, liveToolCalls int, alreadyNudged bool, submitToolName string) bool {
	if threshold <= 0 || alreadyNudged || submitToolName == "" {
		return false
	}
	return liveToolCalls >= threshold
}

// resourceLabelsToAttributes converts resourceLabels map to OpenTelemetry attributes
func (e *executor[Request, Response]) resourceLabelsToAttributes() []attribute.KeyValue {
	if len(e.resourceLabels) == 0 {
		return nil
	}
	attrs := make([]attribute.KeyValue, 0, len(e.resourceLabels))
	for k, v := range e.resourceLabels {
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}

// recordTokenMetrics records token usage with optional enrichment
func (e *executor[Request, Response]) recordTokenMetrics(ctx context.Context, inputTokens, outputTokens int64) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordTokens(ctx, e.modelName, inputTokens, outputTokens, attrs...)
}

// recordCacheMetrics records prompt cache token usage with optional enrichment
func (e *executor[Request, Response]) recordCacheMetrics(ctx context.Context, cacheRead, cacheCreation int64) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordCacheTokens(ctx, e.modelName, cacheRead, cacheCreation, attrs...)
}

// recordToolCall records a tool call metric with optional enrichment
func (e *executor[Request, Response]) recordToolCall(ctx context.Context, toolName string) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordToolCall(ctx, e.modelName, toolName, attrs...)
}

// recordTurns records the number of turns used and, when limitExceeded is true,
// increments the turn_limit_exceeded counter.
func (e *executor[Request, Response]) recordTurns(ctx context.Context, turns int, limitExceeded bool) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordTurns(ctx, e.modelName, turns, limitExceeded, attrs...)
}

// recordAPIRequest counts a single Claude API attempt with a response_code
// derived from err. Call this after every retry-wrapped API call (whether the
// final outcome was success, retryable failure, or non-retryable failure) so
// the counter sees one increment per HTTP attempt.
func (e *executor[Request, Response]) recordAPIRequest(ctx context.Context, err error) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordAPIRequest(ctx, e.modelName, responseCodeAttr(responseCodeFromError(err)), attrs...)
}

// withAPIRequestCounter extends cfg.OnAttemptError to also count each retried
// API attempt in genai.api.requests. The retry loop only invokes
// OnAttemptError for retryable errors that will be retried, so this captures
// the intermediate attempts that retry.RetryWithBackoff would otherwise hide;
// the final attempt is counted by the caller after RetryWithBackoff returns.
func (e *executor[Request, Response]) withAPIRequestCounter(ctx context.Context, cfg retry.RetryConfig) retry.RetryConfig {
	base := cfg.OnAttemptError
	cfg.OnAttemptError = func(err error) {
		if base != nil {
			base(err)
		}
		e.recordAPIRequest(ctx, err)
	}
	return cfg
}

// samplingParamsRemovedPrefixes lists model-name prefixes for which the
// Anthropic API removed the sampling parameters (temperature, top_p, top_k)
// AND the extended-thinking budget parameter (thinking.type="enabled",
// budget_tokens=N) in favor of adaptive thinking. Opus 4.7 introduced this
// surface; Opus 4.8 and Fable 5 share it.
// See: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7#sampling-parameters-removed
var samplingParamsRemovedPrefixes = []string{
	"claude-opus-4-7",
	"claude-opus-4-8",
	"claude-fable-5",
	"claude-sonnet-5",
}

// supportsSamplingParams reports whether the Anthropic API accepts the
// temperature, top_p, and top_k parameters for the given model. Models with
// the removed surface return a 400 ("`temperature` is deprecated for this
// model.") when any of these is set to a non-default value.
func supportsSamplingParams(modelName string) bool {
	for _, prefix := range samplingParamsRemovedPrefixes {
		if strings.HasPrefix(modelName, prefix) {
			return false
		}
	}
	return true
}

// supportsExtendedThinkingBudget reports whether the Anthropic API accepts the
// extended-thinking budget parameter (thinking.type="enabled", budget_tokens=N)
// for the given model. The models that removed sampling params removed the
// budget parameter with them.
// See: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7#extended-thinking-budgets-removed
func supportsExtendedThinkingBudget(modelName string) bool {
	return supportsSamplingParams(modelName)
}
