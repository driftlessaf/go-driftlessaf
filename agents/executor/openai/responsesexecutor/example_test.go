/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor_test

import (
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/openai/responsesexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/openai/openai-go/responses"
)

type request struct{}

func (request) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) { return p, nil }

func ExampleNew() {
	type answer struct {
		Summary string `json:"summary"`
	}
	prompt, err := promptbuilder.NewPrompt("Inspect the supplied synthetic fixture and submit your result.")
	if err != nil {
		panic(err)
	}
	// The application supplies a transport-configured service. Construction
	// itself never loads credentials or makes an inference request.
	var service responses.ResponseService
	_, err = responsesexecutor.New[request](service, responsesexecutor.Config[answer]{
		Model: "us.openai.gpt-5.6-sol", UserPrompt: prompt,
		Attribution: agenttrace.Attribution{ProviderName: "aws.bedrock", System: "aws.bedrock", LogicalModel: "gpt-5.6-sol", Protocol: "openai-responses"},
		MaxTurns:    5, MaxTokens: 2048, ToolCallConcurrency: 1,
		RequestTimeout:   15 * time.Minute,
		ExecutionTimeout: time.Hour,
	})
	fmt.Println(err)
	// Output: <nil>
}
