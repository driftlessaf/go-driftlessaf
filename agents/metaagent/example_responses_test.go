/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/openai/openai-go/responses"
)

func ExampleNewOpenAIResponsesBinding() {
	selection := modelrouter.Selection{Provider: "test-provider", LogicalModel: "gpt-5.6-sol"}
	routes, err := modelrouter.NewRegistry(modelrouter.Route{Selection: selection, Protocol: modelrouter.ProtocolOpenAIResponses, ProviderModelID: "fixture-deployment", Attribution: modelrouter.Attribution{ProviderName: "test-provider", LegacySystem: "test-provider"}})
	if err != nil {
		panic(err)
	}
	plan, err := routes.Resolve(selection)
	if err != nil {
		panic(err)
	}
	binding, err := metaagent.NewOpenAIResponsesBinding(plan, responses.ResponseService{}, map[string]string{"region": "fixture"})
	if err != nil {
		panic(err)
	}
	_ = binding.Responses() // A typed service, not a Chat Completions client.
	fmt.Println(binding.Plan().Protocol(), binding.ResourceLabels()["region"])
	// Output: openai-responses fixture
}

func ExampleNewOpenAIResponsesAdapterRegistry() {
	// Supply an authenticated service in the application adapter. This example
	// only registers it: neither credentials nor network are accessed here.
	adapter := func(_ context.Context, plan modelrouter.Plan) (metaagent.OpenAIResponsesBinding, error) {
		return metaagent.NewOpenAIResponsesBinding(plan, responses.ResponseService{}, nil)
	}
	_, err := metaagent.NewOpenAIResponsesAdapterRegistry(metaagent.OpenAIResponsesRegistration{Provider: "test-provider", Adapter: adapter})
	fmt.Println(err)
	// Output: <nil>
}
