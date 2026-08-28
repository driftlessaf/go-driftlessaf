/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"cmp"
	"slices"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestWithAttribution(t *testing.T) {
	t.Parallel()

	want := agenttrace.Attribution{
		ProviderName: "test.provider",
		System:       "test.system",
		LogicalModel: "test-model",
		Protocol:     "test-protocol",
	}
	e := &executor[*testBindable, *testResponse]{}
	if err := WithAttribution[*testBindable, *testResponse](want)(e); err != nil {
		t.Fatalf("WithAttribution: %v", err)
	}
	if e.attribution != want {
		t.Errorf("attribution = %#v, want %#v", e.attribution, want)
	}

	invalid := want
	invalid.ProviderName = "unsafe\nprovider"
	if err := WithAttribution[*testBindable, *testResponse](invalid)(e); err == nil {
		t.Fatal("WithAttribution accepted a provider name with a control character")
	}
}

func TestWithMaxTokens(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	tests := []struct {
		name    string
		tokens  int64
		wantErr bool
	}{
		{name: "typical", tokens: 16000, wantErr: false},
		{name: "old default", tokens: 32000, wantErr: false},
		{name: "above old cap", tokens: 64000, wantErr: false},
		{name: "at ceiling", tokens: 128000, wantErr: false},
		{name: "above ceiling", tokens: 128001, wantErr: true},
		{name: "zero", tokens: 0, wantErr: true},
		{name: "negative", tokens: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New[*testBindable, *testResponse](
				anthropic.Client{}, // client not needed for option validation
				prompt,
				WithMaxTokens[*testBindable, *testResponse](tt.tokens),
			)

			if (err != nil) != tt.wantErr {
				t.Errorf("WithMaxTokens(%d) error = %v, wantErr %v", tt.tokens, err, tt.wantErr)
			}
		})
	}
}

func TestWithModel(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	tests := []struct {
		name    string
		model   string
		wantErr bool
	}{
		{name: "canonical Claude ID", model: "claude-sonnet-5"},
		{name: "AWS Bedrock Claude ID", model: "anthropic.claude-sonnet-5"},
		{name: "other provider", model: "gemini-3-pro", wantErr: true},
		{name: "empty", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New[*testBindable, *testResponse](
				anthropic.Client{},
				prompt,
				WithModel[*testBindable, *testResponse](tt.model),
			)
			if (err != nil) != tt.wantErr {
				t.Errorf("WithModel(%q) error = %v, wantErr %v", tt.model, err, tt.wantErr)
			}
		})
	}
}

func TestWithMaxTurns(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	tests := []struct {
		name    string
		turns   int
		wantErr bool
	}{
		{name: "valid turns", turns: 10, wantErr: false},
		{name: "one turn", turns: 1, wantErr: false},
		{name: "large turns", turns: 100, wantErr: false},
		{name: "zero turns", turns: 0, wantErr: true},
		{name: "negative turns", turns: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New[*testBindable, *testResponse](
				anthropic.Client{}, // client not needed for option validation
				prompt,
				WithMaxTurns[*testBindable, *testResponse](tt.turns),
			)

			if (err != nil) != tt.wantErr {
				t.Errorf("WithMaxTurns(%d) error = %v, wantErr %v", tt.turns, err, tt.wantErr)
			}
		})
	}
}

func TestDefaultMaxTurns(t *testing.T) {
	t.Parallel()

	if DefaultMaxTurns <= 0 {
		t.Errorf("DefaultMaxTurns = %d, want > 0", DefaultMaxTurns)
	}
}

func TestMaxTurnsApplied(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	// Without option: should get default
	exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e := exec.(*executor[*testBindable, *testResponse])
	if e.maxTurns != DefaultMaxTurns {
		t.Errorf("default maxTurns = %d, want %d", e.maxTurns, DefaultMaxTurns)
	}

	// With option: should override
	exec2, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt,
		WithMaxTurns[*testBindable, *testResponse](25),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e2 := exec2.(*executor[*testBindable, *testResponse])
	if e2.maxTurns != 25 {
		t.Errorf("custom maxTurns = %d, want 25", e2.maxTurns)
	}
}

func TestCacheControlDefault(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	// Default: cacheControl should be true (enabled by default)
	exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e := exec.(*executor[*testBindable, *testResponse])
	if !e.cacheControl {
		t.Error("default cacheControl = false, want true (prompt caching should be on by default)")
	}

	// WithoutCacheControl: should disable
	exec2, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt,
		WithoutCacheControl[*testBindable, *testResponse](),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e2 := exec2.(*executor[*testBindable, *testResponse])
	if e2.cacheControl {
		t.Error("WithoutCacheControl() cacheControl = true, want false")
	}
}

func TestToolDefinitionsSorted(t *testing.T) {
	t.Parallel()

	// Build tools from a map (non-deterministic order)
	tools := map[string]struct {
		name string
	}{
		"zebra":  {name: "zebra"},
		"alpha":  {name: "alpha"},
		"middle": {name: "middle"},
		"beta":   {name: "beta"},
	}

	// Run multiple times to verify sorting overcomes map randomness
	for range 10 {
		defs := make([]anthropic.ToolUnionParam, 0, len(tools))
		for name := range tools {
			defs = append(defs, anthropic.ToolUnionParam{
				OfTool: &anthropic.ToolParam{Name: name},
			})
		}

		// Apply the same sort the executor uses
		slices.SortFunc(defs, func(a, b anthropic.ToolUnionParam) int {
			return cmp.Compare(a.OfTool.Name, b.OfTool.Name)
		})

		expected := []string{"alpha", "beta", "middle", "zebra"}
		for i, def := range defs {
			if def.OfTool.Name != expected[i] {
				t.Errorf("tool[%d] = %q, want %q", i, def.OfTool.Name, expected[i])
			}
		}
	}
}

// testBindable implements promptbuilder.Bindable for testing.
type testBindable struct{}

func (t *testBindable) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

// testResponse is a simple response type for testing.
type testResponse struct {
	Result string `json:"result"`
}

func TestWithProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		opts            []Option[*testBindable, *testResponse]
		wantProvider    Provider
		wantMetricName  string
		wantTraceSystem string
		wantErr         bool
	}{{
		name:            "anthropic first-party",
		opts:            []Option[*testBindable, *testResponse]{WithProvider[*testBindable, *testResponse](ProviderAnthropic)},
		wantProvider:    ProviderAnthropic,
		wantMetricName:  "anthropic",
		wantTraceSystem: "anthropic",
	}, {
		name:            "AWS Bedrock Mantle",
		opts:            []Option[*testBindable, *testResponse]{WithProvider[*testBindable, *testResponse](ProviderBedrock)},
		wantProvider:    ProviderBedrock,
		wantMetricName:  "aws.bedrock",
		wantTraceSystem: "aws.bedrock",
	}, {
		name:            "explicit vertex",
		opts:            []Option[*testBindable, *testResponse]{WithProvider[*testBindable, *testResponse](ProviderVertex)},
		wantProvider:    ProviderVertex,
		wantMetricName:  "gcp.vertex_ai",
		wantTraceSystem: "google.vertex",
	}, {
		name:    "unknown provider errors",
		opts:    []Option[*testBindable, *testResponse]{WithProvider[*testBindable, *testResponse](Provider("unknown"))},
		wantErr: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &executor[*testBindable, *testResponse]{
				provider: ProviderVertex,
				attribution: agenttrace.Attribution{
					LogicalModel: "reasoning",
					Protocol:     "anthropic-messages",
				},
			}
			var err error
			for _, opt := range tc.opts {
				if applyErr := opt(e); applyErr != nil {
					err = applyErr
				}
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("WithProvider: got nil error, want validation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("applying options: %v", err)
			}
			if e.provider != tc.wantProvider {
				t.Errorf("provider: got = %q, want = %q", e.provider, tc.wantProvider)
			}
			if got := e.provider.metricName(); got != tc.wantMetricName {
				t.Errorf("metricName: got = %q, want = %q", got, tc.wantMetricName)
			}
			if got := e.provider.traceSystem(); got != tc.wantTraceSystem {
				t.Errorf("traceSystem: got = %q, want = %q", got, tc.wantTraceSystem)
			}
			if got := e.attribution.ProviderName; got != tc.wantMetricName {
				t.Errorf("attribution provider: got = %q, want = %q", got, tc.wantMetricName)
			}
			if got := e.attribution.System; got != tc.wantTraceSystem {
				t.Errorf("attribution system: got = %q, want = %q", got, tc.wantTraceSystem)
			}
			if got := e.attribution.LogicalModel; got != "reasoning" {
				t.Errorf("logical model changed: got = %q, want = %q", got, "reasoning")
			}
			if got := e.attribution.Protocol; got != "anthropic-messages" {
				t.Errorf("protocol changed: got = %q, want = %q", got, "anthropic-messages")
			}
		})
	}
}

// TestDefaultProvider verifies New's contract directly: an executor built
// without WithProvider defaults to Vertex, matching anthropicauth.NewClient's
// fallback when no federation config is present.
func TestDefaultProvider(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	got, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e, ok := got.(*executor[*testBindable, *testResponse])
	if !ok {
		t.Fatalf("New() returned %T, want *executor", got)
	}
	if e.provider != ProviderVertex {
		t.Errorf("default provider: got = %q, want = %q", e.provider, ProviderVertex)
	}
	if got, want := e.attribution.ProviderName, "gcp.vertex_ai"; got != want {
		t.Errorf("default attribution provider: got = %q, want = %q", got, want)
	}
	if got, want := e.attribution.System, agenttrace.SystemGoogleVertex; got != want {
		t.Errorf("default attribution system: got = %q, want = %q", got, want)
	}
}
