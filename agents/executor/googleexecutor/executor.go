/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/result"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/chainguard-dev/clog"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"
	"google.golang.org/genai"
)

// Interface defines the contract for Google AI executors
type Interface[Request promptbuilder.Bindable, Response any] interface {
	// Execute runs the Google AI conversation with the given request and tools
	// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
	Execute(ctx context.Context, request Request, tools map[string]googletool.Metadata[Response], seedToolCalls ...*genai.FunctionCall) (Response, error)
}

// DefaultMaxTurns is the default maximum number of conversation turns (LLM
// round-trips) before the executor aborts. Each turn corresponds to one
// Gemini API call. This prevents runaway loops when the model keeps calling
// tools without converging on a result.
const DefaultMaxTurns = 200

// DefaultToolCallConcurrency is the default bound on how many of a single
// turn's tool calls run concurrently. Models routinely emit several
// independent function calls in one turn (parallel function calling);
// dispatching their handlers concurrently cuts wall-clock latency. Override
// with WithToolCallConcurrency — a value of 1 restores strictly sequential
// dispatch.
const DefaultToolCallConcurrency = 10

// executor is the private implementation of Interface
type executor[Request promptbuilder.Bindable, Response any] struct {
	client             *genai.Client
	prompt             *promptbuilder.Prompt
	model              string
	temperature        float32
	maxOutputTokens    int32
	maxTurns           int // maximum conversation turns before aborting
	systemInstructions *promptbuilder.Prompt

	// userPromptSuffix, when non-nil, is a static operator-authored prompt
	// appended to the built user prompt with a blank-line separator. See
	// WithUserPromptSuffix; the request is never bound into it.
	userPromptSuffix *promptbuilder.Prompt

	responseMIMEType string
	responseSchema   *genai.Schema
	thinkingBudget   *int32                        // nil = disabled, non-nil = enabled with budget
	submitTool       googletool.Metadata[Response] // opt-in: set via WithSubmitResultProvider
	genaiMetrics     *metrics.GenAI                // OpenTelemetry metrics for token usage and tool calls
	retryConfig      retry.RetryConfig             // retry configuration for transient Vertex AI errors
	resourceLabels   map[string]string             // resource labels for GCP billing attribution

	// cacheControl enables Vertex AI context caching. When true, the executor
	// creates a CachedContent resource containing system instructions and tool
	// definitions, then references it in GenerateContentConfig instead of setting
	// SystemInstruction and Tools directly. Cached tokens are served at reduced
	// cost with a configurable TTL.
	// Enabled by default — disable with WithoutCacheControl() if needed.
	// See: https://cloud.google.com/vertex-ai/generative-ai/docs/context-cache/context-cache-overview
	cacheControl bool

	// cacheTTL is the time-to-live for cached content resources.
	// Default: 30 minutes. Can be overridden via WithCacheTTL().
	cacheTTL time.Duration

	// cacheMu protects all cache-related mutable state below.
	cacheMu             sync.Mutex
	cachedContentName   string    // resource name of active CachedContent ("" = none)
	cachedContentExpiry time.Time // when the current cache expires

	// toolCallConcurrency bounds how many of a single turn's function calls run
	// concurrently when the model emits more than one (parallel function
	// calling). Defaults to DefaultToolCallConcurrency. A value of 1 runs the
	// turn's function calls strictly in order, one at a time. Concurrent
	// dispatch is only safe when the registered tool handlers are themselves
	// safe for concurrent use (they share the trace, which is safe). Set via
	// WithToolCallConcurrency.
	toolCallConcurrency int
}

// New creates a new Google AI executor with the given configuration
func New[Request promptbuilder.Bindable, Response any](
	client *genai.Client,
	prompt *promptbuilder.Prompt,
	options ...Option[Request, Response],
) (Interface[Request, Response], error) {
	if prompt == nil {
		return nil, errors.New("prompt is required")
	}

	// Create GenAI metrics for observability
	// Uses a unified meter across all executors, with model name as a dimension
	genaiMetrics := metrics.NewGenAI("chainguard.ai.agents")

	// Create executor with defaults
	exec := &executor[Request, Response]{
		client:              client,
		prompt:              prompt,
		model:               "gemini-2.5-flash", // Default to Gemini 2.5 Flash
		temperature:         0.1,                // Default temperature for consistency
		maxOutputTokens:     8192,               // Default max tokens
		maxTurns:            DefaultMaxTurns,    // Default max conversation turns
		genaiMetrics:        genaiMetrics,
		retryConfig:         retry.DefaultRetryConfig(), // Default retry config for rate limit handling
		cacheControl:        true,                       // Context caching on by default — see cacheControl field comment
		cacheTTL:            30 * time.Minute,           // Default cache TTL
		toolCallConcurrency: DefaultToolCallConcurrency, // Concurrent tool dispatch by default — see WithToolCallConcurrency
	}

	// Apply options
	for _, opt := range options {
		if err := opt(exec); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	return exec, nil
}

// Execute implements the Interface
// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
func (e *executor[Request, Response]) Execute(
	ctx context.Context,
	request Request,
	tools map[string]googletool.Metadata[Response],
	seedToolCalls ...*genai.FunctionCall,
) (resp Response, err error) {
	// Guard against incompatible combination: thinking mode + seed tool calls.
	// When ThinkingConfig is set, Gemini requires that model turns with FunctionCall
	// parts also include Thought parts. Synthetic seed turns have no Thought parts,
	// causing a Vertex AI API validation error.
	if e.thinkingBudget != nil && len(seedToolCalls) > 0 {
		return resp, errors.New("seed tool calls cannot be used with thinking mode: " +
			"synthetic function call history is missing required thought blocks")
	}

	// Bind the request to the prompt
	boundPrompt, err := request.Bind(e.prompt)
	if err != nil {
		return resp, fmt.Errorf("failed to bind request to prompt: %w", err)
	}

	// Build the prompt string
	prompt, err := boundPrompt.Build()
	if err != nil {
		return resp, fmt.Errorf("failed to build prompt: %w", err)
	}

	// Append the static user prompt suffix, when configured. Gemini has no
	// per-block prompt-cache semantics (context caching covers system
	// instructions and tools via CachedContent), so plain concatenation
	// preserves the prompt content without any block layout.
	prompt, err = appendUserPromptSuffix(prompt, e.userPromptSuffix)
	if err != nil {
		return resp, err
	}

	// Start a trace for this execution — done completes and records
	trace, done := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() {
		done(resp, err)
	}()

	// Merge submit_result tool if configured (opt-in via WithSubmitResultProvider)
	if e.submitTool.Handler != nil {
		mergedTools := make(map[string]googletool.Metadata[Response], len(tools)+1)
		maps.Copy(mergedTools, tools)

		name := "submit_result"
		if e.submitTool.Definition != nil && e.submitTool.Definition.Name != "" {
			name = e.submitTool.Definition.Name
		}
		if _, exists := mergedTools[name]; !exists {
			mergedTools[name] = e.submitTool
		}
		tools = mergedTools
	}

	// Build tool definitions, sorted by name for deterministic ordering.
	//
	// Why sort? Vertex AI context caching hashes the cached content (system
	// instructions + tools). Go maps iterate in non-deterministic order, so
	// without sorting, the tool definitions could serialize differently on each
	// call — producing different cache content even though the tools haven't
	// changed. Sorting by name ensures stable content across calls.
	toolDeclarations := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, meta := range tools {
		toolDeclarations = append(toolDeclarations, meta.Definition)
	}
	slices.SortFunc(toolDeclarations, func(a, b *genai.FunctionDeclaration) int {
		return cmp.Compare(a.Name, b.Name)
	})

	// Create generation config
	config := &genai.GenerateContentConfig{
		Temperature:     ptr(e.temperature),
		MaxOutputTokens: e.maxOutputTokens,
		Labels:          e.resourceLabels,
	}

	// Build system instruction content
	var systemInstruction *genai.Content
	if e.systemInstructions != nil {
		systemPrompt, err := e.systemInstructions.Build()
		if err != nil {
			return resp, fmt.Errorf("building system prompt: %w", err)
		}
		systemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: systemPrompt}},
		}
	}

	// Build tools
	var genaiTools []*genai.Tool
	if len(toolDeclarations) > 0 {
		genaiTools = []*genai.Tool{{FunctionDeclarations: toolDeclarations}}
	}

	// Attempt to use cached content for system instructions + tools.
	// When using CachedContent, SystemInstruction and Tools MUST NOT be set
	// on GenerateContentConfig — they are already in the cache.
	usedCache := false
	if e.cacheControl && (systemInstruction != nil || len(genaiTools) > 0) {
		cacheName, err := e.getOrCreateCache(ctx, systemInstruction, genaiTools)
		if err != nil {
			clog.WarnContext(ctx, "Failed to create cached content, falling back to non-cached mode", "error", err)
		} else {
			config.CachedContent = cacheName
			usedCache = true
		}
	}

	// Fall back to inline system instruction and tools if not using cache.
	if !usedCache {
		if systemInstruction != nil {
			config.SystemInstruction = systemInstruction
		}
		if len(genaiTools) > 0 {
			config.Tools = genaiTools
		}
	}

	// Add response MIME type if provided
	if e.responseMIMEType != "" {
		config.ResponseMIMEType = e.responseMIMEType
	}

	// Add response schema if provided
	if e.responseSchema != nil {
		config.ResponseSchema = e.responseSchema
	}

	// Add thinking configuration if enabled
	if e.thinkingBudget != nil {
		config.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingBudget:  e.thinkingBudget,
		}
	}

	// Create a new chat session with optional seed messages
	clog.InfoContext(ctx, "Creating Google AI chat session",
		"model", e.model,
	)

	// Pre-execute seed tool calls and prepare history
	// Build complete history, then split: use first n-1 for chat creation, send last via SendMessage
	history := make([]*genai.Content, 0, 1+len(seedToolCalls)*2)

	// Add initial user prompt to history
	history = append(history, &genai.Content{
		Role: "user",
		Parts: []*genai.Part{{
			Text: prompt,
		}},
	})

	// finalResult stores the result if a tool sets it
	var finalResult Response
	finalResultPtr := &finalResult

	// Execute seed tool calls and build complete history
	for _, call := range seedToolCalls {
		clog.InfoContext(ctx, "Pre-executing seed tool call", "tool", call.Name, "id", call.ID)

		// Execute the tool call
		var result *genai.FunctionResponse
		if meta, ok := tools[call.Name]; ok {
			result = meta.Handler(ctx, call, trace, finalResultPtr)
			// Preserve the model's universal `reasoning` argument on the
			// recorded call (handlers record curated param maps that drop it).
			if r, ok := call.Args["reasoning"].(string); ok {
				trace.AttachToolCallReasoning(call.ID, r)
			}
		} else {
			clog.ErrorContext(ctx, "Unknown seed tool requested", "tool", call.Name)
			trace.BadToolCall(call.ID, call.Name, call.Args, fmt.Errorf("unknown tool: %q", call.Name))
			result = &genai.FunctionResponse{
				ID:   call.ID,
				Name: call.Name,
				Response: map[string]any{
					"error": fmt.Sprintf("unknown tool: %q", call.Name),
				},
			}
		}

		// Check if a tool set the final result during seed execution
		if !reflect.ValueOf(finalResult).IsZero() {
			clog.InfoContext(ctx, "Seed tool set final result, exiting immediately")
			return finalResult, nil
		}

		// Add model response with function call and function response
		history = append(history, &genai.Content{
			Role: "model",
			Parts: []*genai.Part{{
				FunctionCall: call,
			}},
		}, &genai.Content{
			Role: "user",
			Parts: []*genai.Part{{
				FunctionResponse: result,
			}},
		})
	}

	// Create chat with first n-1 messages, send last message separately
	chat, err := e.client.Chats.Create(ctx, e.model, config, history[:len(history)-1])
	if err != nil {
		return resp, fmt.Errorf("failed to create chat with model %q: %w", e.model, err)
	}

	// Send final message to get response with retry for transient errors
	clog.InfoContext(ctx, "Sending final message")
	response, err := retry.RetryWithBackoff(ctx, e.withAPIRequestCounter(ctx, e.retryConfig), "send_initial_message", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
		return chat.Send(ctx, history[len(history)-1].Parts...)
	})
	e.recordAPIRequest(ctx, err)
	if err != nil {
		if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableVertexError, "Vertex AI"); requeueErr != nil {
			return resp, requeueErr
		}
		return resp, fmt.Errorf("failed to send final message: %w", err)
	}

	if response != nil && response.UsageMetadata != nil {
		e.recordTokenMetrics(ctx, response.UsageMetadata)
	}

	// Handle the conversation loop with bounded turns to prevent runaway executions.
	var responseText string
	// The named err return is load-bearing: the deferred Fail call below reads
	// it at function exit. Every error path must use `return ..., err` (or set
	// the named err before bare-returning) — a bare return inside a nested
	// block where err is shadowed via `:=` would silently bypass Fail.
	executeTurn := func(turn int, response *genai.GenerateContentResponse) (_ Response, _ *genai.GenerateContentResponse, _ bool, err error) {
		var zero Response
		llmTurn := trace.BeginTurn(turn, agenttrace.SystemGoogleVertex, e.model)
		defer func() {
			if err != nil {
				llmTurn.Fail(err)
			}
			llmTurn.End()
		}()

		// Per-turn retry config wires transient API errors that the retry
		// recovers from into the turn's Errors list. Without this, retries
		// that eventually succeed leave no trace of the transients in BQ.
		// Used by all three in-turn retry call sites below.
		// Also count every retried attempt in genai.api.requests; the final
		// attempt is counted by each call site below after RetryWithBackoff.
		turnCfg := e.retryConfig
		turnCfg.OnAttemptError = llmTurn.RecordError
		turnCfg = e.withAPIRequestCounter(ctx, turnCfg)

		// Record tokens for the response being processed in this turn.
		// Unlike claude/openai executors (which record tokens right after
		// each API call), the Google SDK loop processes the *previous*
		// iteration's response at the top of the next turn — so for turn 0
		// this captures the initial API call, and for later turns it captures
		// the response from the preceding tool/redirect call.
		if response != nil && response.UsageMetadata != nil {
			llmTurn.RecordTokens(
				int64(response.UsageMetadata.PromptTokenCount),
				int64(response.UsageMetadata.CandidatesTokenCount),
			)
			// Vertex reports cache hits via CachedContentTokenCount; cache
			// writes are not separately billed for Gemini (see cost view),
			// so CacheCreationTokens stays 0 here. A future "cache write
			// events" metric would have to source from getOrCreateCache
			// (the only place a Gemini cache create actually happens) —
			// turns[] sees only the read-side hits.
			if e.cacheControl && response.UsageMetadata.CachedContentTokenCount > 0 {
				llmTurn.RecordCacheTokens(int64(response.UsageMetadata.CachedContentTokenCount), 0)
			}
		}

		clog.InfoContext(ctx, "Received response from model", "candidates_count", len(response.Candidates))

		if len(response.Candidates) == 0 {
			return zero, nil, true, errors.New("no content generated - no candidates")
		}

		candidate := response.Candidates[0]

		// Capture per-turn LLM payload alongside RecordTokens. Unlike the
		// claude/openai executors (which capture params.Messages before the
		// SDK call), the genai chat handle owns conversation state — so we
		// read it from chat.History(false), which at this point includes the
		// just-returned response. PromptMessages therefore carries the full
		// context the model saw *plus* its own response; Completion holds
		// just the response candidate. The redundancy is benign — SQL queries
		// pick the column matching the question, and prompt_hash stays unique
		// per (context, response) pair. History(false) is uncurated so
		// malformed/filtered turns persist verbatim, matching the security
		// posture documented on agenttrace.SpanEventType. Gated inside on
		// agenttrace.WithPayloadsEnabled.
		if err := llmTurn.RecordRequest(chat.History(false)); err != nil {
			clog.WarnContext(ctx, "failed to record llm prompt payload", "error", err)
		}
		if err := llmTurn.RecordResponse(candidate.Content); err != nil {
			clog.WarnContext(ctx, "failed to record llm response payload", "error", err)
		}

		// Check for safety/blocking reasons before processing content
		if candidate.FinishReason == genai.FinishReasonSafety {
			clog.WarnContext(ctx, "Gemini blocked response due to safety filters",
				"finish_message", candidate.FinishMessage,
				"safety_ratings", candidate.SafetyRatings)
			return zero, nil, true, fmt.Errorf("response blocked by safety filters: %s", candidate.FinishMessage)
		}

		if candidate.FinishReason == genai.FinishReasonRecitation {
			clog.WarnContext(ctx, "Gemini blocked response due to recitation concerns",
				"finish_message", candidate.FinishMessage)
			return zero, nil, true, fmt.Errorf("response blocked due to recitation: %s", candidate.FinishMessage)
		}

		// Check for MAX_TOKENS - response was truncated due to output token limit
		if candidate.FinishReason == genai.FinishReasonMaxTokens {
			clog.WarnContext(ctx, "Gemini hit max output tokens limit, asking for more concise response",
				"finish_message", candidate.FinishMessage,
				"turn", turn)

			// Ask the model to provide a more concise version
			retryMsg := genai.Part{Text: "Your response exceeded the maximum output token limit. Please provide a more concise response that focuses on the most critical information while still completing the task."}
			retryResp, err := retry.RetryWithBackoff(ctx, turnCfg, "send_max_tokens_retry", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
				return chat.SendMessage(ctx, retryMsg)
			})
			e.recordAPIRequest(ctx, err)
			if err != nil {
				if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableVertexError, "Vertex AI"); requeueErr != nil {
					return zero, nil, true, requeueErr
				}
				return zero, nil, true, fmt.Errorf("failed to send retry message after hitting max tokens: %w", err)
			}

			// Record metrics for retry call
			if retryResp != nil && retryResp.UsageMetadata != nil {
				e.recordTokenMetrics(ctx, retryResp.UsageMetadata)
			}

			// Continue with the new response
			return zero, retryResp, false, nil
		}

		// Check for malformed function call
		if candidate.FinishReason == genai.FinishReasonMalformedFunctionCall {
			clog.WarnContext(ctx, "Model attempted a malformed function call, asking it to retry",
				"finish_message", candidate.FinishMessage)

			// Build available function names for retry message
			var funcNames []string
			for _, decl := range toolDeclarations {
				funcNames = append(funcNames, decl.Name)
			}

			// Send a message asking the model to try again with retry for transient errors
			retryMsg := genai.Part{Text: fmt.Sprintf("The function call was malformed. Please try again using the available functions: %v", funcNames)}
			retryResp, err := retry.RetryWithBackoff(ctx, turnCfg, "send_malformed_retry", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
				return chat.SendMessage(ctx, retryMsg)
			})
			e.recordAPIRequest(ctx, err)
			if err != nil {
				if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableVertexError, "Vertex AI"); requeueErr != nil {
					return zero, nil, true, requeueErr
				}
				return zero, nil, true, fmt.Errorf("failed to send retry message after malformed function call: %w", err)
			}

			// Record metrics for retry call. Per-turn token recording for
			// retryResp happens at the top of the next executeTurn iteration.
			if retryResp != nil && retryResp.UsageMetadata != nil {
				e.recordTokenMetrics(ctx, retryResp.UsageMetadata)
			}

			// Continue with the new response
			return zero, retryResp, false, nil
		}

		if candidate.Content == nil {
			clog.ErrorContext(ctx, "Gemini returned nil content",
				"finish_reason", candidate.FinishReason,
				"finish_message", candidate.FinishMessage)
			return zero, nil, true, fmt.Errorf("no content generated - candidate content is nil (finish_reason=%v, finish_message=%q)",
				candidate.FinishReason, candidate.FinishMessage)
		}

		if len(candidate.Content.Parts) == 0 {
			clog.ErrorContext(ctx, "Gemini returned empty parts",
				"finish_reason", candidate.FinishReason,
				"finish_message", candidate.FinishMessage)
			return zero, nil, true, fmt.Errorf("no content generated - no parts in candidate (finish_reason=%v, finish_message=%q)",
				candidate.FinishReason, candidate.FinishMessage)
		}

		// Check for function calls or text
		var toolCalls []*genai.FunctionCall
		var hasText bool

		for i, part := range candidate.Content.Parts {
			switch {
			case part.Thought:
				// Gated on the WithPayloadsEnabled opt-in inside AppendReasoning:
				// raw thinking is confidential completion content, not structural
				// metadata, so it is only captured when payloads are enabled.
				trace.AppendReasoning(agenttrace.ReasoningContent{
					Thinking: part.Text,
				})
				clog.InfoContext(ctx, "Found thought part",
					"part_index", i,
					"thinking_length", len(part.Text))
			case part.Text != "":
				responseText = part.Text
				hasText = true
				clog.InfoContext(ctx, "Found text part",
					"part_index", i,
					"text_length", len(part.Text))
			case part.FunctionCall != nil:
				toolCalls = append(toolCalls, part.FunctionCall)
				clog.InfoContext(ctx, "Found function call part",
					"part_index", i,
					"function_name", part.FunctionCall.Name,
					"function_id", part.FunctionCall.ID)
			default:
				clog.WarnContext(ctx, "Found part with unexpected content", "part_index", i)
			}
		}

		// If there are tool calls, execute them and send responses
		if len(toolCalls) > 0 {
			// executeToolCall runs a single function call and returns its
			// response part. The handler writes any terminal result into
			// resultPtr; the sequential path passes the shared finalResultPtr,
			// while the concurrent path passes a per-call slot so handlers never
			// race on the same pointer.
			executeToolCall := func(call *genai.FunctionCall, resultPtr *Response) *genai.Part {
				kvs := make([]any, 0, 4+2*len(call.Args))
				kvs = append(kvs, "tool", call.Name, "id", call.ID)
				for k, v := range call.Args {
					kvs = append(kvs, "args."+k, v)
				}
				clog.InfoContext(ctx, "Executing tool call", kvs...)

				// Record tool call metric
				e.recordToolCall(ctx, call.Name)

				// Find and execute the handler for this tool
				var toolResponse *genai.FunctionResponse
				toolMeta, found := tools[call.Name]
				if !found {
					clog.ErrorContext(ctx, "Unknown function call requested by model", "function", call.Name)
					toolResponse = googletool.Error(call, "Unknown function: %s", call.Name)

					// Record bad tool call for unknown function
					trace.BadToolCall(call.ID, call.Name, call.Args, fmt.Errorf("unknown function: %q", call.Name))
				} else {
					// Execute the tool handler
					toolResponse = toolMeta.Handler(ctx, call, trace, resultPtr)
					// Preserve the model's universal `reasoning` argument on
					// the recorded call (handlers record curated param maps
					// that drop it).
					if r, ok := call.Args["reasoning"].(string); ok {
						trace.AttachToolCallReasoning(call.ID, r)
					}
				}

				return &genai.Part{FunctionResponse: toolResponse}
			}

			// Dispatch the turn's function calls under a bounded pool. The model
			// may emit several independent function calls in one turn (parallel
			// function calling, which the API maps back by id); a concurrency of 1
			// runs them strictly in order, higher values run them concurrently.
			// Each handler writes into its own result slot so the shared
			// finalResultPtr is never raced; the response parts are then consumed
			// in the model's original order and the first terminal result (in
			// order) ends the run. Tool handlers must be safe for concurrent use
			// when concurrency exceeds 1.
			perCallResults := make([]Response, len(toolCalls))
			parts := make([]*genai.Part, len(toolCalls))

			g := new(errgroup.Group)
			g.SetLimit(max(1, e.toolCallConcurrency))
			for i, call := range toolCalls {
				g.Go(func() error {
					parts[i] = executeToolCall(call, &perCallResults[i])
					return nil
				})
			}
			_ = g.Wait()

			var toolResponseParts []*genai.Part
			for i := range toolCalls {
				if !reflect.ValueOf(perCallResults[i]).IsZero() {
					clog.InfoContext(ctx, "Tool set final result, exiting conversation loop", "turns_completed", turn+1)
					e.recordTurns(ctx, turn+1, false)
					return perCallResults[i], nil, true, nil
				}
				toolResponseParts = append(toolResponseParts, parts[i])
			}

			// Send tool responses back to the chat with retry for transient errors
			nextResponse, err := retry.RetryWithBackoff(ctx, turnCfg, "send_tool_responses", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
				return chat.Send(ctx, toolResponseParts...)
			})
			e.recordAPIRequest(ctx, err)
			if err != nil {
				if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableVertexError, "Vertex AI"); requeueErr != nil {
					return zero, nil, true, requeueErr
				}
				return zero, nil, true, fmt.Errorf("failed to send tool responses: %w", err)
			}

			if nextResponse != nil && nextResponse.UsageMetadata != nil {
				e.recordTokenMetrics(ctx, nextResponse.UsageMetadata)
			}
			return zero, nextResponse, false, nil
		}

		// When submit_result is configured, it is the only valid exit path.
		// If the model responds with text instead of calling submit_result,
		// redirect it back to use the tool.
		if e.submitTool.Handler != nil && hasText {
			clog.WarnContext(ctx, "Model responded with text instead of calling submit_result, redirecting")
			e.recordToolCall(ctx, "submit_result_redirect")

			redirectResp, err := retry.RetryWithBackoff(ctx, turnCfg, "send_submit_redirect", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
				return chat.SendMessage(ctx, genai.Part{
					Text: "You must call the submit_result tool to return your response. Do not respond with plain text. If you encountered an error or cannot complete the task, call submit_result with an appropriate error or summary.",
				})
			})
			e.recordAPIRequest(ctx, err)
			if err != nil {
				if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableVertexError, "Vertex AI"); requeueErr != nil {
					return zero, nil, true, requeueErr
				}
				return zero, nil, true, fmt.Errorf("failed to send submit_result redirect: %w", err)
			}

			if redirectResp != nil && redirectResp.UsageMetadata != nil {
				e.recordTokenMetrics(ctx, redirectResp.UsageMetadata)
			}

			return zero, redirectResp, false, nil
		}

		// Fallback: parse text response as JSON when submit_result is not configured
		if hasText {
			extractedResponse, err := result.Extract[Response](responseText)
			if err != nil {
				clog.ErrorContext(ctx, "Failed to parse AI response",
					"response", responseText,
					"error", err)
				return zero, nil, true, fmt.Errorf("failed to parse AI response: %w", err)
			}
			clog.InfoContext(ctx, "Successfully completed Google AI agent execution", "turns_completed", turn+1)
			e.recordTurns(ctx, turn+1, false)
			return extractedResponse, nil, true, nil
		}

		// Unexpected state
		clog.ErrorContext(ctx, "Unexpected response format - no text and no tool calls")
		return zero, nil, true, errors.New("unexpected response format from model")
	}

	for turn := range e.maxTurns {
		result, nextResp, done, err := executeTurn(turn, response)
		// done=true on all terminal paths (including errors); || err != nil is a
		// safety net in case a future path sets err without setting done.
		if done || err != nil {
			return result, err
		}
		if nextResp == nil {
			return resp, errors.New("retry returned nil response")
		}
		response = nextResp
	}

	clog.ErrorContext(ctx, "Agent exceeded maximum conversation turns", "max_turns", e.maxTurns)
	e.recordTurns(ctx, e.maxTurns, true)
	// The final unprocessed response carries real billable tokens (the
	// model already generated them). Without this synthetic turn the
	// loop would exit and only OTel metrics would see them — turns[]
	// is the source of truth for cost analysis (DEV-1140), so leaving
	// them off would silently undercount maxTurns-exhausted runs.
	if response != nil && response.UsageMetadata != nil {
		llmTurn := trace.BeginTurn(e.maxTurns, agenttrace.SystemGoogleVertex, e.model)
		llmTurn.RecordTokens(
			int64(response.UsageMetadata.PromptTokenCount),
			int64(response.UsageMetadata.CandidatesTokenCount),
		)
		if e.cacheControl && response.UsageMetadata.CachedContentTokenCount > 0 {
			llmTurn.RecordCacheTokens(int64(response.UsageMetadata.CachedContentTokenCount), 0)
		}
		llmTurn.End()
	}
	return resp, fmt.Errorf("agent exceeded maximum conversation turns (%d)", e.maxTurns)
}

// getOrCreateCache returns the name of a valid CachedContent, creating one if
// needed. It is safe for concurrent use. On cache creation it records
// cache_creation metrics so the cost of the write is visible in dashboards.
func (e *executor[Request, Response]) getOrCreateCache(ctx context.Context, systemInstruction *genai.Content, tools []*genai.Tool) (string, error) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()

	// Cache validity is TTL-only — tool set changes between calls are not detected.
	// Callers must ensure a stable tool set for the lifetime of this executor;
	// use WithoutCacheControl() if the tool set varies per call.
	if e.cachedContentName != "" && time.Now().Add(time.Minute).Before(e.cachedContentExpiry) {
		return e.cachedContentName, nil
	}

	cached, err := e.client.Caches.Create(ctx, e.model, &genai.CreateCachedContentConfig{
		SystemInstruction: systemInstruction,
		Tools:             tools,
		TTL:               e.cacheTTL,
		DisplayName:       fmt.Sprintf("driftlessaf-%s", e.model),
	})
	// Caches.Create is a Vertex API call subject to the same quotas as
	// chat.Send; count it on the same counter so a cache-creation 429 storm
	// shows up on the rate-limit dashboard alongside chat-send 429s.
	e.recordAPIRequest(ctx, err)
	if err != nil {
		return "", fmt.Errorf("creating cached content: %w", err)
	}

	e.cachedContentName = cached.Name
	e.cachedContentExpiry = cached.ExpireTime

	// Record cache creation metrics and log.
	var totalTokenCount int32
	if cached.UsageMetadata != nil {
		totalTokenCount = cached.UsageMetadata.TotalTokenCount
		if totalTokenCount > 0 {
			e.recordCacheMetrics(ctx, 0, int64(totalTokenCount))
		}
	}

	clog.InfoContext(ctx, "Created context cache",
		"cache_name", cached.Name,
		"expire_time", cached.ExpireTime,
		"total_token_count", totalTokenCount,
	)

	return cached.Name, nil
}

// appendUserPromptSuffix appends the built suffix to the prompt with a
// blank-line separator. A nil suffix returns the prompt unchanged. The suffix
// must be fully bound; a Build failure (for example an unbound placeholder)
// is returned wrapped.
func appendUserPromptSuffix(prompt string, suffix *promptbuilder.Prompt) (string, error) {
	if suffix == nil {
		return prompt, nil
	}
	built, err := suffix.Build()
	if err != nil {
		return "", fmt.Errorf("failed to build user prompt suffix: %w", err)
	}
	return prompt + "\n\n" + built, nil
}

// ptr is a helper function to create a pointer to a value
func ptr[T any](v T) *T {
	return &v
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

// recordTokenMetrics records token usage with optional enrichment.
// When context caching is active and the response includes cached tokens,
// OTel cache metrics are also recorded. Per-turn cache token attribution
// onto the trace happens in executeTurn (LLMTurn.RecordCacheTokens) — this
// path only emits the OTel metrics counter.
func (e *executor[Request, Response]) recordTokenMetrics(ctx context.Context, usage *genai.GenerateContentResponseUsageMetadata) {
	if usage == nil {
		return
	}

	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "gcp.vertex_ai"))
	e.genaiMetrics.RecordTokens(ctx, e.model, int64(usage.PromptTokenCount), int64(usage.CandidatesTokenCount), attrs...)

	if e.cacheControl && usage.CachedContentTokenCount > 0 {
		e.recordCacheMetrics(ctx, int64(usage.CachedContentTokenCount), 0)
		clog.DebugContext(ctx, "Prompt cache metrics",
			"cache_read_tokens", usage.CachedContentTokenCount)
	}
}

// recordCacheMetrics records context cache token usage with optional enrichment.
func (e *executor[Request, Response]) recordCacheMetrics(ctx context.Context, cacheRead, cacheCreation int64) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "gcp.vertex_ai"))
	e.genaiMetrics.RecordCacheTokens(ctx, e.model, cacheRead, cacheCreation, attrs...)
}

// recordToolCall records a tool call metric with optional enrichment
func (e *executor[Request, Response]) recordToolCall(ctx context.Context, toolName string) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "gcp.vertex_ai"))
	e.genaiMetrics.RecordToolCall(ctx, e.model, toolName, attrs...)
}

// recordTurns records the number of turns used and, when limitExceeded is true,
// increments the turn_limit_exceeded counter.
func (e *executor[Request, Response]) recordTurns(ctx context.Context, turns int, limitExceeded bool) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "gcp.vertex_ai"))
	e.genaiMetrics.RecordTurns(ctx, e.model, turns, limitExceeded, attrs...)
}

// recordAPIRequest counts a single Vertex API attempt with a response_code
// derived from err. Call this after every retry-wrapped API call (whether the
// final outcome was success, retryable failure, or non-retryable failure) so
// the counter sees one increment per HTTP attempt — matching what GCP's
// serviceruntime metric sees on its side of the wire.
func (e *executor[Request, Response]) recordAPIRequest(ctx context.Context, err error) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "gcp.vertex_ai"))
	e.genaiMetrics.RecordAPIRequest(ctx, e.model, responseCodeAttr(responseCodeFromError(err)), attrs...)
}

// withAPIRequestCounter extends cfg.OnAttemptError to also count each retried
// API attempt in genai.api.requests. The retry loop only invokes
// OnAttemptError for retryable errors that will be retried, so this captures
// the intermediate attempts that retry.RetryWithBackoff would otherwise hide;
// the final attempt is counted by the caller after RetryWithBackoff returns.
// Together they give exactly one increment per HTTP attempt.
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
