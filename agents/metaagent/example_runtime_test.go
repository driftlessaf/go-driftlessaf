/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

func exampleRuntime() *metaagent.Runtime {
	runtime, err := metaagent.NewRuntime([]modelrouter.Route{{
		Selection:       modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "gemini-2.5-flash"},
		Protocol:        modelrouter.ProtocolGoogleGenAI,
		ProviderModelID: "gemini-2.5-flash",
		Attribution:     modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: "google.vertex"},
		Capabilities:    modelrouter.Capabilities{ToolCalling: true, TerminalSubmission: true},
	}}, metaagent.VertexBackend("", metaagent.VertexConfig{ProjectID: "my-project"}))
	if err != nil {
		panic(err)
	}
	return runtime
}

func ExampleNewRuntime() {
	runtime := exampleRuntime()
	config := metaagent.TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "gemini-2.5-flash", Region: "global"}
	first, selection, err := runtime.Target(config).Resolve()
	if err != nil {
		panic(err)
	}
	second, _, err := runtime.Target(config).Resolve()
	if err != nil {
		panic(err)
	}
	fmt.Println(selection.Provider, selection.LogicalModel, first == second)
	// Output: vertex gemini-2.5-flash true
}

func ExampleRuntime_Target() {
	target := exampleRuntime().Target(metaagent.TargetConfig{
		Provider: modelrouter.ProviderVertexAI,
		Model:    "gemini-2.5-flash",
		Region:   "global",
	})
	_, selection, err := target.Resolve()
	if err != nil {
		panic(err)
	}
	fmt.Println(selection.Provider)
	// Output: vertex
}

func ExampleTarget_Resolve() {
	_, selection, err := exampleRuntime().Target(metaagent.TargetConfig{
		Provider: modelrouter.ProviderVertexAI, Model: "gemini-2.5-flash", Region: "global",
	}).Resolve()
	if err != nil {
		panic(err)
	}
	fmt.Println(selection.LogicalModel)
	// Output: gemini-2.5-flash
}

func ExampleNewWithTarget() {
	target := exampleRuntime().Target(metaagent.TargetConfig{
		Provider: modelrouter.ProviderVertexAI, Model: "gemini-2.5-flash", Region: "global",
	})
	// This step binds the adapter using Application Default Credentials.
	_, err := metaagent.NewWithTarget[*request](context.Background(), target, metaagent.Config[*response, toolcall.EmptyTools]{
		UserPrompt: promptbuilder.MustNewPrompt("Answer the request."),
		Tools:      toolcall.NewEmptyToolsProvider[*response](),
	})
	if err != nil {
		fmt.Println(err)
	}
}

func ExampleNewBedrockRuntimeRouter() {
	// Supply exact routes validated for your AWS account and selected API.
	route := modelrouter.Route{
		Selection:       modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-4-6"},
		Protocol:        modelrouter.ProtocolAnthropicMessages,
		ProviderModelID: "anthropic.claude-sonnet-4-6",
		Attribution:     modelrouter.Attribution{ProviderName: "aws.bedrock", LegacySystem: "aws.bedrock"},
		Capabilities:    modelrouter.Capabilities{ToolCalling: true, TerminalSubmission: true},
	}
	_, err := metaagent.NewBedrockRuntimeRouter(awsauth.Config{Region: "us-east-1", Profile: "development"}, route)
	fmt.Println(err)
	// Output: <nil>
}

func ExampleVertexBackend() {
	_, err := metaagent.NewRuntime(nil, metaagent.VertexBackend("primary", metaagent.VertexConfig{ProjectID: "my-project"}))
	fmt.Println(err)
	// Output: <nil>
}

func ExampleBedrockBackend() {
	_, err := metaagent.NewRuntime(nil, metaagent.BedrockBackend("", awsauth.Config{Region: "us-east-1"}))
	fmt.Println(err)
	// Output: <nil>
}

func ExampleAnthropicBackend() {
	_, err := metaagent.NewRuntime(nil, metaagent.AnthropicBackend("", anthropicauth.Config{FederationRuleID: "rule", OrganizationID: "org"}))
	fmt.Println(err)
	// Output: <nil>
}

func ExampleNewAdapterBackend() {
	calls := 0
	backend := metaagent.NewAdapterBackend("account", "example-provider", func(region string) (metaagent.AdapterRegistrations, error) {
		calls++
		// Return adapters capturing this account's configuration and region.
		return metaagent.AdapterRegistrations{}, nil
	})
	_, err := metaagent.NewRuntime(nil, backend)
	fmt.Println(calls, err)
	// Output: 0 <nil>
}
