/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/effort"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/internal/calltiming"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/model"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/schema"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// Config configures a credential-free Responses executor. Zero numeric limits
// select defaults: 200 turns, 32768 output tokens, and 10 concurrent tools.
// Concurrent tools must synchronize shared state themselves. Reasoning and cache
// tokens are subsets of output and input tokens, respectively, not extra usage.
type Config[Response any] struct {
	Model               string
	Attribution         agenttrace.Attribution
	UserPrompt          *promptbuilder.Prompt
	SystemInstructions  *promptbuilder.Prompt
	UserPromptSuffix    *promptbuilder.Prompt
	MaxTurns            int
	ToolCallConcurrency int
	MaxTokens           int64
	// Effort is sent verbatim. Routed callers validate model support against
	// the route's capabilities; direct callers must select a supported level.
	Effort           effort.Level
	Submit           submitresult.Options[Response]
	ResultValidators []callbacks.ResultValidator[Response]
	ResourceLabels   map[string]string

	// RequestTimeout bounds each streaming HTTP attempt. Zero inherits the
	// execution deadline. This is a total timeout, not an idle timeout.
	RequestTimeout time.Duration

	// ExecutionTimeout bounds the conversation, including tools and retries.
	// Zero selects 30 minutes. A shorter caller deadline always wins. Each
	// Resume gets a fresh ExecutionTimeout.
	ExecutionTimeout time.Duration

	// SuspendTool advertises a held-out tool that pauses the run. When the
	// model calls it, Execute waits for the turn's other calls to finish and
	// returns a *checkpoint.Suspension; Resumer continues the conversation.
	// Its name must differ from the submit tool's and from every caller tool.
	// Nil disables suspension.
	SuspendTool *responses.FunctionToolParam

	// SuspendOnSubmit makes an accepted submission pause the run instead of
	// ending it: Execute returns the accepted Response with a
	// *checkpoint.Suspension whose Reason is checkpoint.ReasonSubmitted and
	// whose pending call is the submit call. ResultResumer continues with the
	// caller's review as that call's result. Only a turn's first submission
	// is reviewed, and a suspend call in the same turn is answered as not
	// asked. It is part of the resume config digest.
	SuspendOnSubmit bool
}

// Interface executes a native Responses conversation with provider-neutral tools.
type Interface[Request promptbuilder.Bindable, Response any] interface {
	Execute(context.Context, Request, map[string]toolcall.Tool[Response]) (Response, error)
}

type executor[Request promptbuilder.Bindable, Response any] struct {
	client   responses.ResponseService
	config   Config[Response]
	submit   submitresult.ResponsesMetadata[Response]
	recorder *telemetry.Recorder
}

// Empty requested means provider default; empty reported means unavailable.
type reasoningEffortEvidence struct {
	Turn      int          `json:"turn"`
	Requested effort.Level `json:"requested"`
	Reported  effort.Level `json:"reported"`
}

// New constructs a native Responses executor using an already configured client.
// It does not access the network or inspect ambient authentication variables.
func New[Request promptbuilder.Bindable, Response any](client responses.ResponseService, cfg Config[Response]) (Interface[Request, Response], error) {
	if cfg.UserPrompt == nil || strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("responses requires a prompt and model")
	}
	if err := cfg.Attribution.Validate(); err != nil {
		return nil, fmt.Errorf("responses attribution: %w", err)
	}
	if cfg.Attribution.Protocol != "openai-responses" {
		return nil, errors.New("responses requires openai-responses attribution")
	}
	if cfg.MaxTurns < 0 || cfg.ToolCallConcurrency < 0 || cfg.MaxTokens < 0 {
		return nil, errors.New("responses limits cannot be negative")
	}
	if cfg.RequestTimeout < 0 || cfg.ExecutionTimeout < 0 {
		return nil, errors.New("responses timeouts cannot be negative")
	}
	if cfg.Effort != "" {
		if err := cfg.Effort.Validate(); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.ResultValidators {
		if v == nil {
			return nil, errors.New("responses result validator cannot be nil")
		}
	}
	cfg.MaxTurns = cmp.Or(cfg.MaxTurns, 200)
	cfg.MaxTokens = cmp.Or(cfg.MaxTokens, int64(32768))
	cfg.ToolCallConcurrency = cmp.Or(cfg.ToolCallConcurrency, 10)
	cfg.ExecutionTimeout = cmp.Or(cfg.ExecutionTimeout, 30*time.Minute)
	cfg.ResultValidators = append([]callbacks.ResultValidator[Response]{schema.ResultValidator[Response]()}, cfg.ResultValidators...)
	cfg.ResourceLabels = execshared.DefaultResourceLabels(cfg.ResourceLabels)
	submit, err := submitresult.ResponsesTool(cfg.Submit)
	if err != nil {
		return nil, err
	}
	if cfg.SuspendTool != nil {
		if err := execshared.ValidateSuspendToolName(cfg.SuspendTool.Name, submit.Definition.Name); err != nil {
			return nil, err
		}
		// The executor must not see later caller edits to the definition.
		suspendTool := *cfg.SuspendTool
		cfg.SuspendTool = &suspendTool
	}
	return &executor[Request, Response]{
		client:   client,
		config:   cfg,
		submit:   submit,
		recorder: telemetry.NewRecorder(metrics.NewGenAI("chainguard.ai.agents"), cfg.Model, cfg.Attribution.LogicalModel, cfg.Attribution.ProviderName, cfg.ResourceLabels, statusCode),
	}, nil
}

func (e *executor[Request, Response]) Execute(ctx context.Context, request Request, tools map[string]toolcall.Tool[Response]) (response Response, err error) {
	// A caller's shorter deadline still wins. A provider that stops sending
	// events cannot leave a run alive indefinitely.
	ctx, cancel := context.WithTimeout(ctx, e.config.ExecutionTimeout)
	defer cancel()
	bound, err := request.Bind(e.config.UserPrompt)
	if err != nil {
		return response, err
	}
	prompt, err := bound.Build()
	if err != nil {
		return response, err
	}
	suffix, err := promptbuilder.BindSuffix(e.config.UserPromptSuffix, request)
	if err != nil {
		return response, err
	}
	prompt, err = execshared.AppendUserPromptSuffix(prompt, suffix)
	if err != nil {
		return response, err
	}
	trace, done := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() { done(response, err) }()
	params, err := e.staticParams(tools)
	if err != nil {
		return response, err
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: responses.ResponseInputParam{responses.ResponseInputItemParamOfMessage(prompt, "user")}}
	return e.runConversation(ctx, trace, params, tools, make(map[string]struct{}), 0, e.config.MaxTurns)
}

// staticParams builds every request field except the input: the
// turn-invariant request the resume config digest covers.
func (e *executor[Request, Response]) staticParams(tools map[string]toolcall.Tool[Response]) (responses.ResponseNewParams, error) {
	defs, err := e.definitions(tools)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params := responses.ResponseNewParams{
		Model:             e.config.Model,
		Store:             param.NewOpt(false),
		MaxOutputTokens:   param.NewOpt(e.config.MaxTokens),
		ParallelToolCalls: param.NewOpt(true),
		Include:           []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		Tools:             defs,
	}
	if e.config.SystemInstructions != nil {
		instructions, err := e.config.SystemInstructions.Build()
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Instructions = param.NewOpt(instructions)
	}
	if e.config.Effort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(e.config.Effort)}
	}
	// Only function calls advance the loop, so a text-only turn is unusable.
	// Models that reject a forced tool_choice keep the provider default and
	// rely on the continuation nudge instead.
	if !model.Resolve(e.config.Attribution.LogicalModel).AutomaticToolChoiceOnly {
		params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsRequired)}
	}
	return params, nil
}

// runConversation drives the bounded turn loop shared by Execute and Resume.
// params carries the conversation so far as its input. Turns are numbered
// from startTurn, and the run gets budget of them. seen holds every function
// call ID already in the conversation.
func (e *executor[Request, Response]) runConversation(
	ctx context.Context,
	trace *agenttrace.Trace[Response],
	params responses.ResponseNewParams,
	tools map[string]toolcall.Tool[Response],
	seen map[string]struct{},
	startTurn, budget int,
) (response Response, err error) {
	// isSubmit and isSuspend route the terminal submit tool and the suspend
	// tool. Both are held out: they run only after every sibling handler has
	// finished, so a submission's validators see the finished state and a
	// suspension captures the siblings' real results. A caller tool that
	// shadows the suspend tool fails the run before any request.
	isSubmit, isSuspend, heldOut, err := execshared.HeldOutPartition(tools, e.submit.Definition.Name, true, e.suspendToolName(), e.config.SuspendTool != nil)
	if err != nil {
		return response, err
	}
	route := toolRouting{isSubmit: isSubmit, isSuspend: isSuspend, heldOut: heldOut}
	// Structural evidence survives payload truncation and does not contain
	// prompts or reasoning text. Write only after all tool workers have joined.
	var reasoningEvidence []reasoningEffortEvidence
	defer func() { trace.Metadata["responses_reasoning_effort"] = reasoningEvidence }()
	invalid := 0
	turns := 0
	limitExceeded := false
	defer func() { e.recorder.RecordTurns(ctx, turns, limitExceeded) }()
	// turnLimit is where this run's budget runs out. A suspension carries what
	// is left of it, not a figure recomputed from the live MaxTurns.
	turnLimit := startTurn + budget
	for index := startTurn; index < turnLimit; index++ {
		if err := ctx.Err(); err != nil {
			return response, err
		}
		turn := trace.BeginTurnWithAttribution(index, e.config.Model, e.config.Attribution)
		turns++
		accepted, usable, turnErr := func() (bool, bool, error) {
			defer turn.End()
			requestPayload, err := safePayload(params)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			if err := turn.RecordRequest(requestPayload); err != nil {
				turn.Fail(err)
				return false, false, err
			}
			rc := retry.RetryConfig{MaxRetries: 2, BaseBackoff: time.Second, MaxBackoff: 4 * time.Second, OnAttemptError: turn.RecordError}
			// Each attempt gets a fresh timer, so the latency recorded below is
			// the answering attempt's.
			var timer *calltiming.Timer
			out, err := retry.RetryWithBackoff(ctx, rc, "responses request", retryable, func() (*responses.Response, error) {
				timer = new(calltiming.Timer)
				out, err := e.stream(ctx, params, timer)
				e.recorder.RecordAPIRequest(ctx, err)
				return out, err
			})
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			execshared.RecordCallLatency(ctx, trace, turn, timer.Latency())
			turn.RecordTokens(out.Usage.InputTokens, out.Usage.OutputTokens)
			turn.RecordCacheTokens(out.Usage.InputTokensDetails.CachedTokens, out.Usage.InputTokensDetails.CacheWriteTokens)
			turn.RecordReasoningTokens(out.Usage.OutputTokensDetails.ReasoningTokens)
			reported := effort.Level(out.Reasoning.Effort)
			switch reported {
			case "", "none", "minimal", effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max:
			default:
				reported = ""
			}
			reasoningEvidence = append(reasoningEvidence, reasoningEffortEvidence{
				Turn: index, Requested: e.config.Effort, Reported: reported,
			})
			e.recorder.RecordTokens(ctx, out.Usage.InputTokens, out.Usage.OutputTokens)
			e.recorder.RecordCacheTokens(ctx, out.Usage.InputTokensDetails.CachedTokens, out.Usage.InputTokensDetails.CacheWriteTokens)
			responsePayload, err := safePayload(out)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			if err := turn.RecordResponse(responsePayload); err != nil {
				turn.Fail(err)
				return false, false, err
			}
			if e.config.Effort != "" && reported != "" && reported != e.config.Effort {
				err := fmt.Errorf("responses reasoning effort mismatch: requested %q, provider reported %q", e.config.Effort, reported)
				turn.Fail(err)
				return false, false, err
			}
			// Preserve native messages, calls, and opaque reasoning. Do not put
			// encrypted reasoning or raw provider payloads in logs or traces.
			items, calls, err := continuation(out, seen)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			params.Input.OfInputItemList = append(params.Input.OfInputItemList, items...)
			result, err := e.dispatch(ctx, calls, tools, route, trace, &response)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			if result.pending >= 0 {
				// A suspension is an intentional halt, so the turn does not
				// fail. The input now ends with this turn's output items.
				susp, err := e.buildSuspension(params.Input.OfInputItemList, calls, result, tools, index, turnLimit, trace.ID)
				if err != nil {
					turn.Fail(err)
					return false, false, err
				}
				return result.accepted, true, susp
			}
			params.Input.OfInputItemList = append(params.Input.OfInputItemList, result.outputs...)
			if len(calls) == 0 {
				params.Input.OfInputItemList = append(params.Input.OfInputItemList, responses.ResponseInputItemParamOfMessage("Continue using the available tools, then submit the complete result with the terminal tool.", "user"))
			}
			return result.accepted, result.usable, nil
		}()
		if turnErr != nil {
			if susp, ok := checkpoint.AsSuspension(turnErr); ok {
				clog.InfoContext(ctx, "Suspending Responses agent execution", "turn", index, "reason", susp.Reason, "tool", susp.PendingToolCalls[0].Name, "id", susp.PendingToolCalls[0].ID)
				trace.Suspend(susp.Reason)
				return response, susp
			}
			if requeue := retry.RequeueIfRetryable(ctx, turnErr, retryable, e.config.Attribution.ProviderName); requeue != nil {
				return response, requeue
			}
			return response, turnErr
		}
		if accepted {
			return response, nil
		}
		if usable {
			invalid = 0
		} else {
			invalid++
		}
		if invalid >= 3 {
			return response, errors.New("responses exceeded three consecutive unusable turns")
		}
	}
	limitExceeded = true
	return response, fmt.Errorf("%w (%d)", agentexecutor.ErrMaxTurns, budget)
}

func (e *executor[Request, Response]) definitions(tools map[string]toolcall.Tool[Response]) ([]responses.ToolUnionParam, error) {
	defs := []responses.ToolUnionParam{{OfFunction: &e.submit.Definition}}
	// A caller tool of the suspend tool's name fails the run in dispatch
	// rather than shadowing it.
	if st := e.config.SuspendTool; st != nil {
		if _, shadowed := tools[st.Name]; !shadowed {
			defs = append(defs, responses.ToolUnionParam{OfFunction: st})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		t := tools[name]
		if name == e.submit.Definition.Name || name == "" || name != t.Def.Name || t.Handler == nil {
			return nil, errors.New("responses tool has an invalid name, handler, or terminal-name collision")
		}
		properties := map[string]any{"reasoning": map[string]any{"type": "string", "description": "Why this tool is needed."}}
		required := []string{"reasoning"}
		for _, p := range t.Def.Parameters {
			properties[p.Name] = toolcall.ParameterToMap(p)
			if p.Required && p.Name != "reasoning" {
				required = append(required, p.Name)
			}
		}
		input := maps.Clone(t.Def.InputSchemaExtensions)
		if input == nil {
			input = make(map[string]any)
		}
		input["type"], input["properties"], input["required"] = "object", properties, required
		if t.Def.InputSchemaDescription != "" {
			input["description"] = t.Def.InputSchemaDescription
		}
		if len(t.Def.InputSchemaDefs) != 0 {
			input["$defs"] = toolcall.SchemaToMap(&toolcall.Schema{Defs: t.Def.InputSchemaDefs})["$defs"]
		}
		defs = append(defs, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{Name: name, Description: param.NewOpt(t.Def.Description), Parameters: input, Strict: param.NewOpt(false)}})
	}
	return defs, nil
}

func continuation(out *responses.Response, seen map[string]struct{}) (responses.ResponseInputParam, []responses.ResponseFunctionToolCall, error) {
	var items responses.ResponseInputParam
	var calls []responses.ResponseFunctionToolCall
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type != "output_text" && c.Type != "refusal" {
					return nil, nil, errors.New("responses returned unsupported message content")
				}
			}
			items = append(items, responses.ResponseInputItemUnionParam{OfOutputMessage: new(item.AsMessage().ToParam())})
		case "reasoning":
			items = append(items, responses.ResponseInputItemUnionParam{OfReasoning: new(item.AsReasoning().ToParam())})
		case "function_call":
			call := item.AsFunctionCall()
			_, repeated := seen[call.CallID]
			if call.CallID == "" || call.Name == "" || repeated || len(calls) >= 128 {
				return nil, nil, errors.New("responses returned invalid or repeated function call identifiers")
			}
			seen[call.CallID] = struct{}{}
			calls = append(calls, call)
			items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCall: new(call.ToParam())})
		default:
			return nil, nil, errors.New("responses returned unsupported output item")
		}
	}
	return items, calls, nil
}

// dispatchResult is one turn's tool results.
type dispatchResult struct {
	accepted, usable bool
	// outputs holds one function_call_output per call, in call order. The
	// pending call's entry is the zero value: its result is what a resume
	// supplies.
	outputs responses.ResponseInputParam
	// pending is the index of the call the run pauses on, or -1.
	pending int
	// reason is the suspension reason when pending is set.
	reason string
}

// toolRouting is a run's held-out partition (see execshared.HeldOutPartition).
type toolRouting struct {
	isSubmit, isSuspend, heldOut func(name string) bool
}

func (e *executor[Request, Response]) dispatch(ctx context.Context, calls []responses.ResponseFunctionToolCall, tools map[string]toolcall.Tool[Response], route toolRouting, trace *agenttrace.Trace[Response], result *Response) (dispatchResult, error) {
	type outcome struct {
		output                    map[string]any
		accepted, usable, suspend bool
		err                       error
	}
	outcomes := make([]outcome, len(calls))
	committed := false // terminal callbacks run sequentially after ordinary tools join
	afterSiblings := func(c responses.ResponseFunctionToolCall) bool { return tools[c.Name].Def.AfterSiblings }
	terminal := func(c responses.ResponseFunctionToolCall) bool { return route.heldOut(c.Name) }
	execshared.DispatchToolCalls(calls, e.config.ToolCallConcurrency, afterSiblings, terminal, func(i int, c responses.ResponseFunctionToolCall) {
		o := &outcomes[i]
		if err := ctx.Err(); err != nil {
			o.err = err
			return
		}
		var args map[string]any
		if json.Unmarshal([]byte(c.Arguments), &args) != nil || args == nil {
			err := errors.New("tool arguments must be a JSON object")
			trace.RejectedToolCall(c.CallID, c.Name, nil, err)
			o.output = map[string]any{"error": err.Error()}
			return
		}
		call := toolcall.ToolCall{ID: c.CallID, Name: c.Name, Args: args}
		e.recorder.RecordToolCall(ctx, c.Name)
		switch {
		case route.isSuspend(c.Name):
			// Its result is the answer a resume supplies.
			o.suspend = true
		case route.isSubmit(c.Name):
			if committed {
				o.output = map[string]any{"error": "a terminal result was already accepted"}
				if e.config.SuspendOnSubmit {
					o.output = map[string]any{"error": "only one submission per turn is reviewed; an earlier submit call in this turn is the one under review"}
				}
				return
			}
			submission := e.submit.Handler(ctx, call, trace)
			o.output, o.accepted, o.err = execshared.GateSubmission(ctx, submission, trace, c.CallID, c.Name, args, e.config.ResultValidators, e.recorder, e.submit.Definition.Name, result)
			committed = o.accepted
		default:
			if t, ok := tools[c.Name]; ok {
				// A regular handler cannot write the shared final result. The
				// terminal validator is the only authority to commit it.
				var scratch Response
				o.output = t.Handler(ctx, call, trace, &scratch)
				o.usable = true
			} else {
				trace.RejectedToolCall(c.CallID, c.Name, args, errors.New("unknown tool"))
				o.output = map[string]any{"error": "unknown tool; use an advertised function"}
			}
		}
	})
	res := dispatchResult{outputs: make(responses.ResponseInputParam, len(calls)), pending: -1}
	submitIdx, suspendIdx := -1, -1
	for i, o := range outcomes {
		if o.err != nil {
			return dispatchResult{}, o.err
		}
		res.accepted = res.accepted || o.accepted
		res.usable = res.usable || o.usable
		output := o.output
		switch {
		case o.accepted && e.config.SuspendOnSubmit:
			// The caller's review replaces the acceptance as this call's
			// result.
			submitIdx = i
			continue
		case o.suspend && suspendIdx < 0:
			suspendIdx = i
			continue
		case o.suspend:
			output = map[string]any{"error": "only one question per turn is asked; an earlier call in this turn is the one awaiting an answer"}
		}
		item, err := encodeOutput(calls[i].CallID, output)
		if err != nil {
			return dispatchResult{}, err
		}
		res.outputs[i] = item
	}
	switch {
	case submitIdx >= 0:
		res.pending, res.reason = submitIdx, checkpoint.ReasonSubmitted
		if suspendIdx >= 0 {
			// The submission overtook the question, but the conversation goes
			// on, so the model must learn it was never sent.
			item, err := encodeOutput(calls[suspendIdx].CallID, map[string]any{
				"status": "not_asked",
				"reason": "you submitted in the same turn, so the question was not sent; ask again if you still need an answer",
			})
			if err != nil {
				return dispatchResult{}, err
			}
			res.outputs[suspendIdx] = item
		}
	case suspendIdx >= 0 && !res.accepted:
		res.pending, res.reason = suspendIdx, checkpoint.ReasonAwaitingAnswer
	}
	return res, nil
}

// encodeOutput encodes one tool result as its function_call_output item.
func encodeOutput(callID string, output map[string]any) (responses.ResponseInputItemUnionParam, error) {
	b, err := json.Marshal(output)
	if err != nil {
		return responses.ResponseInputItemUnionParam{}, errors.New("responses tool result is not JSON encodable")
	}
	if len(b) > maxPayloadBytes {
		return responses.ResponseInputItemUnionParam{}, errors.New("responses tool result exceeds byte limit")
	}
	return functionCallOutput(callID, string(b)), nil
}

// functionCallOutput builds the text result item for one function call. The
// SDK's ResponseInputItemParamOfFunctionCallOutput helper takes only the
// output, so it cannot set the call_id that pairs the result with its call.
func functionCallOutput(callID, output string) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
		CallID: param.NewOpt(callID),
		Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(output)},
	}}
}
