/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

import (
	"context"
	"fmt"

	"github.com/chainguard-dev/clog"

	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type myRequest struct {
	Input string
}

func (r myRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p.BindJSON("input", r.Input)
}

type myResponse struct {
	Summary string `json:"summary"`
}

func ExampleNew() {
	ctx := context.Background()
	prompt := promptbuilder.MustNewPrompt("Summarize: {{input}}")

	client := openai.NewClient(
		option.WithAPIKey("placeholder"),
	)

	exec, err := openaiexecutor.New[myRequest, myResponse](client, prompt,
		openaiexecutor.WithModel[myRequest, myResponse]("google/gemini-2.5-flash"),
		openaiexecutor.WithMaxTokens[myRequest, myResponse](8192),
		openaiexecutor.WithTemperature[myRequest, myResponse](0.1),
	)
	if err != nil {
		clog.FatalContextf(ctx, "%v", err)
	}

	fmt.Printf("executor created: %v\n", exec != nil)
	// Output: executor created: true
}

func ExampleWithModel() {
	ctx := context.Background()
	prompt := promptbuilder.MustNewPrompt("Analyze: {{input}}")

	client := openai.NewClient(
		option.WithAPIKey("placeholder"),
	)

	exec, err := openaiexecutor.New[myRequest, myResponse](client, prompt,
		openaiexecutor.WithModel[myRequest, myResponse]("deepseek-ai/deepseek-v3.2-maas"),
	)
	if err != nil {
		clog.FatalContextf(ctx, "%v", err)
	}

	fmt.Printf("executor created: %v\n", exec != nil)
	// Output: executor created: true
}

func ExampleWithMaxTurns() {
	ctx := context.Background()
	prompt := promptbuilder.MustNewPrompt("Process: {{input}}")

	client := openai.NewClient(
		option.WithAPIKey("placeholder"),
	)

	exec, err := openaiexecutor.New[myRequest, myResponse](client, prompt,
		openaiexecutor.WithMaxTurns[myRequest, myResponse](50),
	)
	if err != nil {
		clog.FatalContextf(ctx, "%v", err)
	}

	fmt.Printf("executor created: %v\n", exec != nil)
	// Output: executor created: true
}

func ExampleWithTemperature() {
	ctx := context.Background()
	prompt := promptbuilder.MustNewPrompt("Summarize: {{input}}")

	client := openai.NewClient(
		option.WithAPIKey("placeholder"),
	)

	exec, err := openaiexecutor.New[myRequest, myResponse](client, prompt,
		openaiexecutor.WithTemperature[myRequest, myResponse](0.5),
	)
	if err != nil {
		clog.FatalContextf(ctx, "%v", err)
	}

	fmt.Printf("executor created: %v\n", exec != nil)
	// Output: executor created: true
}
