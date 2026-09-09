/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func TestBedrockRuntimeValidationBeforeCredentials(t *testing.T) {
	t.Parallel()
	for _, protocol := range []modelrouter.Protocol{
		modelrouter.ProtocolOpenAIChatCompletions,
		modelrouter.ProtocolOpenAIResponses,
		modelrouter.ProtocolAnthropicMessages,
	} {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			for _, test := range []struct {
				name  string
				edit  func(*modelrouter.Route)
				valid bool
			}{
				{"valid plan", func(*modelrouter.Route) {}, true},
				{"wrong provider", func(r *modelrouter.Route) { r.Selection.Provider = modelrouter.ProviderVertexAI }, false},
				{"wrong protocol", func(r *modelrouter.Route) {
					r.Selection.LogicalModel = "gpt-5.6-terra"
					if protocol == modelrouter.ProtocolOpenAIResponses {
						r.Protocol = modelrouter.ProtocolOpenAIChatCompletions
					} else {
						r.Protocol = modelrouter.ProtocolOpenAIResponses
					}
				}, false},
				{"provider attribution", func(r *modelrouter.Route) { r.Attribution.ProviderName = "openai" }, false},
				{"legacy attribution", func(r *modelrouter.Route) { r.Attribution.LegacySystem = "openai" }, false},
				{"zero plan", nil, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					t.Parallel()
					calls := 0
					credentialErr := errors.New("fixture credential discovery failure")
					factory := func(context.Context, awsauth.Config) (bedrockruntime.Client, error) {
						calls++
						return nil, credentialErr
					}
					cfg := awsauth.Config{Region: "us-west-2"}
					var bind func(context.Context, modelrouter.Plan) error
					switch protocol {
					case modelrouter.ProtocolOpenAIResponses:
						adapter, err := newBedrockOpenAIResponsesAdapter(cfg, factory)
						if err != nil {
							t.Fatal(err)
						}
						bind = func(ctx context.Context, plan modelrouter.Plan) error {
							_, err := adapter(ctx, plan)
							return err
						}
					case modelrouter.ProtocolAnthropicMessages:
						adapter, err := newBedrockRuntimeAnthropicMessagesAdapter(cfg, factory)
						if err != nil {
							t.Fatal(err)
						}
						bind = func(ctx context.Context, plan modelrouter.Plan) error {
							_, err := adapter(ctx, plan)
							return err
						}
					default:
						adapter, err := newBedrockOpenAIChatCompletionsAdapter(cfg, factory)
						if err != nil {
							t.Fatal(err)
						}
						bind = func(ctx context.Context, plan modelrouter.Plan) error {
							_, err := adapter(ctx, plan)
							return err
						}
					}
					if calls != 0 {
						t.Fatalf("credential loads at construction: got = %d, want = 0", calls)
					}
					var plan modelrouter.Plan
					if test.edit != nil {
						route := bedrockChatRoute()
						route.Protocol = protocol
						if protocol == modelrouter.ProtocolAnthropicMessages {
							route.Selection.LogicalModel = "claude-sonnet-5"
						}
						test.edit(&route)
						plan = bedrockChatPlan(t, route)
					}
					wantErr, wantCalls := ErrInvalidBinding, 0
					if test.valid {
						wantErr, wantCalls = credentialErr, 1
					}
					if err := bind(t.Context(), plan); !errors.Is(err, wantErr) {
						t.Errorf("binding error: got = %v, want = %v", err, wantErr)
					}
					if calls != wantCalls {
						t.Errorf("credential loads: got = %d, want = %d", calls, wantCalls)
					}
				})
			}
		})
	}
}

func TestBedrockRuntimeAdapterRegions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		region string
		valid  bool
	}{
		{"us-east-1", true},
		{"us-west-2", true},
		{"eu-west-1", true},
		{"", false},
		{"foo", false},
		{"1", false},
		{"us-gov-west-1", false},
		{"cn-north-1", false},
		{"us-iso-east-1", false},
		{"us-isob-east-1", false},
		{"eu-isoe-west-1", false},
		{"us-isof-south-1", false},
		{" us-west-2", false},
		{"us-west-2 ", false},
		{"US-WEST-2", false},
		{"us-west-2/path", false},
		{"us-west-2.example.com", false},
		{"us-" + strings.Repeat("a", 59) + "-1", false},
	} {
		t.Run(test.region, func(t *testing.T) {
			t.Parallel()
			if got := bedrockruntime.ValidRegion(test.region); got != test.valid {
				t.Fatalf("transport region validity: got = %v, want = %v", got, test.valid)
			}
			cfg := awsauth.Config{Region: test.region}
			_, chatErr := NewBedrockOpenAIChatCompletionsAdapter(cfg)
			_, responsesErr := NewBedrockOpenAIResponsesAdapter(cfg)
			_, claudeErr := NewBedrockRuntimeAnthropicMessagesAdapter(cfg)
			for name, err := range map[string]error{"chat": chatErr, "responses": responsesErr, "claude": claudeErr} {
				if test.valid {
					if err != nil {
						t.Errorf("%s constructor: got = %v, want = nil", name, err)
					}
				} else if !errors.Is(err, ErrInvalidAdapter) {
					t.Errorf("%s constructor: got = %v, want = ErrInvalidAdapter", name, err)
				}
			}
		})
	}
}
