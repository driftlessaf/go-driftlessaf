/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ExampleNewWithMessages demonstrates constructing an executor from the
// Messages service shared by Anthropic SDK transports.
func ExampleNewWithMessages() {
	prompt, err := promptbuilder.NewPrompt("Respond to the request.")
	if err != nil {
		fmt.Println(err)
		return
	}

	executor, err := claudeexecutor.NewWithMessages[promptbuilder.Noop, *struct{}](
		anthropic.NewClient(option.WithAPIKey("example")).Messages,
		prompt,
	)
	fmt.Println(executor != nil && err == nil)
	// Output: true
}

// ExampleWithModel demonstrates configuring the Claude model used by the
// executor.
func ExampleWithModel() {
	opt := claudeexecutor.WithModel[promptbuilder.Noop, *struct{}]("claude-3-opus@20240229")
	fmt.Printf("option is nil: %v\n", opt == nil)
	// Output: option is nil: false
}

// ExampleWithMaxTokens demonstrates configuring the maximum number of tokens
// the executor may generate per response.
func ExampleWithMaxTokens() {
	opt := claudeexecutor.WithMaxTokens[promptbuilder.Noop, *struct{}](16000)
	fmt.Printf("option is nil: %v\n", opt == nil)
	// Output: option is nil: false
}

// ExampleWithProvider demonstrates declaring the serving backend so metrics
// (gen_ai.provider.name) and trace turns carry the true provider. Callers
// that build a Bedrock Mantle client pass ProviderBedrock; the default is
// ProviderVertex, matching the Vertex fallback.
func ExampleWithProvider() {
	opt := claudeexecutor.WithProvider[promptbuilder.Noop, *struct{}](claudeexecutor.ProviderBedrock)
	fmt.Printf("option is nil: %v\n", opt == nil)
	// Output: option is nil: false
}
