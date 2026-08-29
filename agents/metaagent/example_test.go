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
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/openai/openai-go"
)

// request is an example request type that implements promptbuilder.Bindable.
type request struct {
	Query string
}

func (r *request) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p.BindXML("query", struct {
		XMLName struct{} `xml:"query"`
		Content string   `xml:",chardata"`
	}{
		Content: r.Query,
	})
}

// response is an example structured response type.
type response struct {
	Answer string `json:"answer"`
}

// ExampleNew demonstrates creating a new meta-agent with model selection.
// New selects the provider implementation based on the model name prefix:
// "gemini-" uses Google's Generative AI SDK, "claude-" uses Anthropic via Vertex AI.
func ExampleNew() {
	ctx := context.Background()

	tools := toolcall.NewEmptyToolsProvider[*response]()
	config := metaagent.Config[*response, toolcall.EmptyTools]{
		Tools: tools,
	}

	// An unsupported model prefix returns an error.
	_, err := metaagent.New[*request](ctx, "my-project", "us-central1", "unknown-model", config)
	if err != nil {
		fmt.Println("error:", err)
	}
	// Output: error: unsupported model: unknown-model (expected gemini-*, claude-*, or publisher/model format)
}

// ExampleNewRouted demonstrates explicit provider selection with a test-only
// provider that reuses the OpenAI Chat Completions protocol.
func ExampleNewRouted() {
	const provider modelrouter.Provider = "example-provider"
	selection := modelrouter.Selection{Provider: provider, LogicalModel: "example/model"}
	routes, err := modelrouter.NewRegistry(modelrouter.Route{
		Selection:       selection,
		Protocol:        modelrouter.ProtocolOpenAIChatCompletions,
		ProviderModelID: "opaque-deployment-id",
		Attribution: modelrouter.Attribution{
			ProviderName: "example.provider",
			LegacySystem: "example.provider",
		},
		Capabilities: modelrouter.Capabilities{
			ToolCalling:        true,
			TerminalSubmission: true,
		},
	})
	if err != nil {
		panic(err)
	}
	adapters, err := metaagent.NewOpenAIChatCompletionsAdapterRegistry(
		metaagent.OpenAIChatCompletionsRegistration{
			Provider: provider,
			Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.OpenAIChatCompletionsBinding, error) {
				// A real application prepares the SDK client with its provider-owned
				// endpoint and credential here, outside the route plan.
				return metaagent.NewOpenAIChatCompletionsBinding(
					plan, openai.Client{}, openaiexecutor.TokenLimitMaxTokens, nil,
				)
			},
		},
	)
	if err != nil {
		panic(err)
	}
	router, err := metaagent.NewRouter(routes, metaagent.AdapterRegistries{OpenAIChatCompletions: adapters})
	if err != nil {
		panic(err)
	}
	prompt, err := promptbuilder.NewPrompt("answer the query")
	if err != nil {
		panic(err)
	}
	agent, err := metaagent.NewRouted[*request](context.Background(), router, selection, metaagent.Config[*response, toolcall.EmptyTools]{
		UserPrompt: prompt,
		Tools:      toolcall.NewEmptyToolsProvider[*response](),
	})
	if err != nil {
		panic(err)
	}

	fmt.Println(agent != nil)
	// Output: true
}

// ExampleNewAnthropicDirectMessagesAdapter demonstrates binding an explicit
// Anthropic workload identity federation configuration to a typed Messages
// adapter. The route plan remains separate and secret-free.
func ExampleNewAnthropicDirectMessagesAdapter() {
	adapter, err := metaagent.NewAnthropicDirectMessagesAdapter(anthropicauth.Config{
		FederationRuleID: "fdrl_0123456789",
		OrganizationID:   "12345678-1234-1234-1234-123456789012",
		Source:           anthropicauth.SourceGoogle,
	})
	if err != nil {
		return
	}
	_ = adapter
}

// ExampleNewBedrockAnthropicMessagesAdapter demonstrates binding explicit AWS
// IAM Identity Center configuration to a typed Bedrock Messages adapter.
func ExampleNewBedrockAnthropicMessagesAdapter() {
	adapter, err := metaagent.NewBedrockAnthropicMessagesAdapter(awsauth.Config{
		Region:  "us-east-1",
		Profile: "engineering-sso",
	})
	if err != nil {
		return
	}
	_ = adapter
}

// ExampleAgent_Execute demonstrates calling Execute on an Agent to run a request.
// Execute sends the request to the model and returns the structured response.
func ExampleAgent_Execute() {
	ctx := context.Background()

	tools := toolcall.NewEmptyToolsProvider[*response]()
	config := metaagent.Config[*response, toolcall.EmptyTools]{
		Tools: tools,
	}

	agent, err := metaagent.New[*request](ctx, "my-project", "us-central1", "gemini-2.0-flash", config)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	req := &request{Query: "What is the answer?"}
	_, err = agent.Execute(ctx, req, toolcall.EmptyTools{})
	if err != nil {
		fmt.Println("error:", err)
	}
}

// executeOnly is an Agent without the Resume capability, standing in for a
// backend that has not grown suspend/resume support yet.
type executeOnly struct{}

func (executeOnly) Execute(context.Context, *request, toolcall.EmptyTools) (*response, error) {
	return &response{}, nil
}

// ExampleAsResumer demonstrates obtaining the opt-in resume capability from a
// constructed agent. AsResumer reports false when the agent's backend does not
// support suspend/resume, so a waker can branch to a fresh run instead of
// assuming every backend can wake a checkpoint. Today only the Claude backend
// (Config.SuspendToolName set) yields a Resumer.
func ExampleAsResumer() {
	var agent metaagent.Agent[*request, *response, toolcall.EmptyTools] = executeOnly{}

	if resumer, ok := metaagent.AsResumer[*request](agent); ok {
		// Resume the parked conversation: answers are keyed by the pending
		// tool-call IDs persisted in the envelope (Envelope.PendingToolCalls).
		_ = resumer
		fmt.Println("resumable")
	} else {
		fmt.Println("not resumable: run from scratch")
	}
	// Output: not resumable: run from scratch
}
