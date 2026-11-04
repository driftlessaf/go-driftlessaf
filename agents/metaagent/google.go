/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// googleAgent implements Agent using Google's Generative AI SDK.
type googleAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor googleexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
}

func newGoogleAgent[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	if config.MaxToolCallsBeforeFinalize != 0 {
		return nil, fmt.Errorf("MaxToolCallsBeforeFinalize (%d) is not supported on the Gemini backend", config.MaxToolCallsBeforeFinalize)
	}

	if config.PromptTokensBeforeFinalize != 0 {
		return nil, fmt.Errorf("PromptTokensBeforeFinalize (%d) is not supported on the Gemini backend", config.PromptTokensBeforeFinalize)
	}
	if config.EnforceInputBudget {
		return nil, errors.New("EnforceInputBudget is not supported on the Gemini backend")
	}
	if config.InputBudgetNoReduction {
		return nil, errors.New("InputBudgetNoReduction is not supported on the Gemini backend")
	}
	if config.CacheTTL != 0 {
		return nil, errors.New("CacheTTL is not supported on the Gemini backend")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  projectID,
		Location: region,
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Google AI client: %w", err)
	}
	return newGoogleAgentWithClient[Req, Resp, CB](googleAgentConstruction{
		client:           client,
		providerModelID:  model,
		logicalModelID:   model,
		applyTemperature: true,
		maxOutputTokens:  65536,
		resourceLabels:   map[string]string{"projectID": projectID, "region": region, "model_name": strings.ToLower(model)},
	}, config)
}

type googleAgentConstruction struct {
	client               *genai.Client
	providerModelID      string
	logicalModelID       string
	routed               bool
	applyTemperature     bool
	maxOutputTokens      int32
	thinkingBudget       int32
	resourceLabels       map[string]string
	attribution          *agenttrace.Attribution
	retryRequestTimeouts bool
}

func newRoutedGoogleAgent[Req promptbuilder.Bindable, Resp, CB any](
	binding GoogleGenAIBinding,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	plan := binding.Plan()
	maxOutputTokens := int32(cmp.Or(config.MaxTokens, int64(65536)))
	attribution := routedAttribution(plan)
	attribution.Serving.Location = binding.ResourceLabels()["region"]
	return newGoogleAgentWithClient[Req, Resp, CB](googleAgentConstruction{
		client:               binding.Client(),
		providerModelID:      plan.ProviderModelID(),
		logicalModelID:       plan.LogicalModel(),
		routed:               true,
		applyTemperature:     plan.Capabilities().SamplingParameters,
		maxOutputTokens:      maxOutputTokens,
		thinkingBudget:       int32(config.ThinkingBudget),
		resourceLabels:       binding.ResourceLabels(),
		attribution:          &attribution,
		retryRequestTimeouts: binding.retryRequestTimeouts,
	}, config)
}

func newGoogleAgentWithClient[Req promptbuilder.Bindable, Resp, CB any](
	construction googleAgentConstruction,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	// Build the terminal submit_result tool. The executor gates accepted
	// submissions on the configured result validators before committing them
	// as the run's final result.
	submitTool, err := submitresult.GoogleTool(submitOptions(config))
	if err != nil {
		return nil, fmt.Errorf("building submit tool: %w", err)
	}

	modelOption := googleexecutor.WithModel[Req, Resp](construction.providerModelID)
	if construction.routed {
		modelOption = googleexecutor.WithRoutedModel[Req, Resp](construction.providerModelID, construction.logicalModelID)
	}
	executorOpts := []googleexecutor.Option[Req, Resp]{
		modelOption,
		googleexecutor.WithMaxOutputTokens[Req, Resp](construction.maxOutputTokens),
		googleexecutor.WithSubmitResultProvider[Req, Resp](func() (googletool.SubmitMetadata[Resp], error) { return submitTool, nil }),
		googleexecutor.WithResourceLabels[Req, Resp](construction.resourceLabels),
	}
	if construction.applyTemperature {
		executorOpts = append(executorOpts, googleexecutor.WithTemperature[Req, Resp](0.2))
	} else {
		executorOpts = append(executorOpts, googleexecutor.WithoutTemperature[Req, Resp]())
	}
	if construction.thinkingBudget > 0 {
		executorOpts = append(executorOpts, googleexecutor.WithThinking[Req, Resp](construction.thinkingBudget))
	}
	if construction.attribution != nil {
		executorOpts = append(executorOpts, googleexecutor.WithAttribution[Req, Resp](*construction.attribution))
	}
	if construction.retryRequestTimeouts {
		executorOpts = append(executorOpts, googleexecutor.WithRetryRequestTimeouts[Req, Resp]())
	}
	for _, v := range config.ResultValidators {
		executorOpts = append(executorOpts, googleexecutor.WithResultValidator[Req, Resp](v))
	}

	if config.MaxTurns > 0 {
		executorOpts = append(executorOpts, googleexecutor.WithMaxTurns[Req, Resp](config.MaxTurns))
	}

	if config.ToolCallConcurrency > 0 {
		executorOpts = append(executorOpts, googleexecutor.WithToolCallConcurrency[Req, Resp](config.ToolCallConcurrency))
	}

	if config.SystemInstructions != nil {
		executorOpts = append(executorOpts, googleexecutor.WithSystemInstructions[Req, Resp](config.SystemInstructions))
	}

	// Gemini has no per-block prompt-cache semantics, so the suffix is simply
	// appended to the built user prompt (see googleexecutor.WithUserPromptSuffix).
	if config.UserPromptSuffix != nil {
		executorOpts = append(executorOpts, googleexecutor.WithUserPromptSuffix[Req, Resp](config.UserPromptSuffix))
	}

	if config.Effort != "" {
		executorOpts = append(executorOpts, googleexecutor.WithEffort[Req, Resp](config.Effort))
	}

	if config.SuspendToolName != "" {
		decl := suspendFunctionDeclaration(config.SuspendToolName, config.SuspendToolDescription)
		executorOpts = append(executorOpts, googleexecutor.WithSuspendTool[Req, Resp](func() (*genai.FunctionDeclaration, error) {
			return decl, nil
		}))
	}
	if config.SuspendOnSubmit {
		executorOpts = append(executorOpts, googleexecutor.WithSuspendOnSubmit[Req, Resp]())
	}

	executor, err := googleexecutor.New[Req, Resp](construction.client, config.UserPrompt, executorOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating Google executor: %w", err)
	}

	return &googleAgent[Req, Resp, CB]{
		executor: executor,
		config:   config,
	}, nil
}

func (a *googleAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building tools: %w", err)
	}
	return a.executor.Execute(ctx, request, googletool.Map(tools))
}

// suspendFunctionDeclaration is the Gemini form of suspendToolParam's schema.
func suspendFunctionDeclaration(name, desc string) *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        name,
		Description: desc,
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				suspendQuestionProperty: {Type: genai.TypeString, Description: suspendQuestionDescription},
				suspendContextProperty:  {Type: genai.TypeString, Description: suspendContextDescription},
			},
			Required: []string{suspendQuestionProperty},
		},
	}
}

// Resume implements Resumer by delegating to the Google executor, whose
// resume capability is kept off googleexecutor.Interface.
func (a *googleAgent[Req, Resp, CB]) Resume(ctx context.Context, env checkpoint.Envelope, answers map[string]string, callbacks CB) (Resp, error) {
	var zero Resp
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		return zero, fmt.Errorf("building tools: %w", err)
	}
	resumer, ok := a.executor.(googleexecutor.Resumer[Req, Resp])
	if !ok {
		return zero, errors.New("google executor does not support resume")
	}
	return resumer.Resume(ctx, env, answers, googletool.Map(tools))
}

// ResumeWithResults implements ResultResumer by delegating to the Google
// executor, as Resume does.
func (a *googleAgent[Req, Resp, CB]) ResumeWithResults(ctx context.Context, env checkpoint.Envelope, results map[string]string, callbacks CB) (Resp, error) {
	var zero Resp
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		return zero, fmt.Errorf("building tools: %w", err)
	}
	resumer, ok := a.executor.(googleexecutor.ResultResumer[Req, Resp])
	if !ok {
		return zero, errors.New("google executor does not support resuming with results")
	}
	return resumer.ResumeWithResults(ctx, env, results, googletool.Map(tools))
}
