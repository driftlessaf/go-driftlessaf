/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"testing"

	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"google.golang.org/genai"
)

func TestVertexGoogleGenAIFlexAdapter(t *testing.T) {
	t.Parallel()

	route := routedTestRoute(
		modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "gemini-3-flash-preview"},
		modelrouter.ProtocolGoogleGenAI,
		"publishers/google/models/gemini-3-flash-preview",
	)
	plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for _, tc := range []struct {
		name         string
		flex         bool
		wantLocation string
		wantHeaders  map[string]string
	}{
		{
			name:         "flex off keeps the target region and sends no flex headers",
			wantLocation: "us-central1",
			wantHeaders:  map[string]string{"X-Vertex-AI-LLM-Request-Type": "", "X-Vertex-AI-LLM-Shared-Request-Type": ""},
		},
		{
			name:         "flex on uses the global location and sends the flex headers",
			flex:         true,
			wantLocation: "global",
			wantHeaders:  map[string]string{"X-Vertex-AI-LLM-Request-Type": "shared", "X-Vertex-AI-LLM-Shared-Request-Type": "flex"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got *genai.ClientConfig
			factory := func(_ context.Context, config *genai.ClientConfig) (*genai.Client, error) {
				got = config
				return &genai.Client{}, nil
			}
			var (
				adapter GoogleGenAIAdapter
				err     error
			)
			if tc.flex {
				adapter, err = newVertexFlexGoogleGenAIAdapter("test-project", factory)
			} else {
				adapter, err = newVertexGoogleGenAIAdapter("test-project", "us-central1", factory)
			}
			if err != nil {
				t.Fatalf("adapter constructor: %v", err)
			}
			binding, err := adapter(t.Context(), plan)
			if err != nil {
				t.Fatalf("adapter: %v", err)
			}
			if got == nil {
				t.Fatal("client factory was not called")
			}
			if got.Location != tc.wantLocation {
				t.Errorf("Location: got = %q, want = %q", got.Location, tc.wantLocation)
			}
			for name, want := range tc.wantHeaders {
				if h := got.HTTPOptions.Headers.Get(name); h != want {
					t.Errorf("header %s: got = %q, want = %q", name, h, want)
				}
			}
			if r := binding.ResourceLabels()["region"]; r != tc.wantLocation {
				t.Errorf("region label: got = %q, want = %q", r, tc.wantLocation)
			}
		})
	}
}

func TestWithVertexFlexRejectsBedrock(t *testing.T) {
	t.Parallel()

	route := modelrouter.Route{
		Selection: modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-4-6"},
		Protocol:  modelrouter.ProtocolAnthropicMessages, ProviderModelID: "us.anthropic.claude-sonnet-4-6",
		Attribution: modelrouter.Attribution{ProviderName: "aws.bedrock", LegacySystem: "aws.bedrock"},
	}
	runtime, err := NewRuntime([]modelrouter.Route{route}, BedrockBackend("", awsauth.Config{Region: "us-east-1"}, WithVertexFlex()))
	if err != nil {
		t.Fatal(err)
	}
	target := runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: route.Selection.LogicalModel, Region: "us-east-1"})
	if _, _, err := target.Resolve(); err == nil {
		t.Error("Resolve: got = nil, want an error for flex on a Bedrock backend")
	}
}

// TestVertexBackendFlexSelectsGlobalBinding pins that the backend option
// reaches the router: only a flex backend binds Gemini to the global location
// when the target names a region.
func TestVertexBackendFlexSelectsGlobalBinding(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))

	route := vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash")
	for _, tc := range []struct {
		name    string
		options []BackendOption
		want    string
	}{
		{name: "flex off keeps the target region", want: "us-central1"},
		{name: "flex on binds the global location", options: []BackendOption{WithVertexFlex()}, want: "global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, err := NewRuntime([]modelrouter.Route{route}, VertexBackend("", VertexConfig{ProjectID: "test-project"}, tc.options...))
			if err != nil {
				t.Fatal(err)
			}
			router, selection, err := runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: route.Selection.LogicalModel, Region: "us-central1"}).Resolve()
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			resolution, err := router.Resolve(selection)
			if err != nil {
				t.Fatalf("router Resolve: %v", err)
			}
			binding, err := resolution.BindGoogleGenAI(t.Context(), modelrouter.Requirements{})
			if err != nil {
				t.Fatalf("BindGoogleGenAI: %v", err)
			}
			if got := binding.ResourceLabels()["region"]; got != tc.want {
				t.Errorf("region label: got = %q, want = %q", got, tc.want)
			}
		})
	}
}
