/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
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
	// Output: error: unsupported model: unknown-model (expected gemini-* or claude-*)
}
