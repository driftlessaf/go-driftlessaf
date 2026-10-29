/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"errors"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/invopop/jsonschema"
	"google.golang.org/genai"
)

// GoogleTool constructs the Google executor metadata for the submit_result tool.
func GoogleTool[Response any](opts Options[Response]) (googletool.SubmitMetadata[Response], error) {
	opts.setDefaults()

	props, required, flat, err := toolParameters(opts, func(s *jsonschema.Schema) (*genai.Schema, error) {
		if g := schemaToGenai(s); g != nil {
			return g, nil
		}
		return nil, errors.New("failed to derive payload schema")
	})
	if err != nil {
		return googletool.SubmitMetadata[Response]{}, err
	}

	handler := func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		return buildOutcome(ctx, opts, flat, trace, call.ID, call.Name, call.Args)
	}

	return googletool.SubmitMetadata[Response]{
		Definition: &genai.FunctionDeclaration{
			Name:        opts.ToolName,
			Description: opts.Description,
			Parameters:  googleInputSchema(props, required, flat, opts.OmitReasoning),
		},
		Handler: handler,
	}, nil
}

// googleInputSchema builds the {reasoning, <payload>} schema for the terminal
// submit_result tool from the parameters toolParameters renders. A flat
// payload's properties keep their struct order, after reasoning, as a nested
// payload's do inside it.
func googleInputSchema(properties map[string]*genai.Schema, required []string, flat *flatParameters, omitReasoning bool) *genai.Schema {
	input := &genai.Schema{
		Type:       genai.TypeObject,
		Properties: properties,
		Required:   required,
	}
	if flat != nil {
		input.PropertyOrdering = flat.names
	}
	if !omitReasoning {
		input.Properties["reasoning"] = &genai.Schema{
			Type:        genai.TypeString,
			Description: reasoningDescription,
		}
		input.Required = append([]string{"reasoning"}, required...)
		if flat != nil {
			input.PropertyOrdering = append([]string{"reasoning"}, flat.names...)
		}
	}
	return input
}

// GoogleToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func GoogleToolForResponse[Response any]() (googletool.SubmitMetadata[Response], error) {
	return GoogleTool(OptionsForResponse[Response]())
}
