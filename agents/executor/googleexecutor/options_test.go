/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"cmp"
	"slices"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"google.golang.org/genai"
)

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
				nil, // client not needed for option validation
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
	exec, err := New[*testBindable, *testResponse](nil, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e := exec.(*executor[*testBindable, *testResponse])
	if e.maxTurns != DefaultMaxTurns {
		t.Errorf("default maxTurns = %d, want %d", e.maxTurns, DefaultMaxTurns)
	}

	// With option: should override
	exec2, err := New[*testBindable, *testResponse](nil, prompt,
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
	exec, err := New[*testBindable, *testResponse](nil, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e := exec.(*executor[*testBindable, *testResponse])
	if !e.cacheControl {
		t.Error("default cacheControl = false, want true (context caching should be on by default)")
	}
	if e.cacheTTL != 30*time.Minute {
		t.Errorf("default cacheTTL = %v, want 30m", e.cacheTTL)
	}

	// WithoutCacheControl: should disable
	exec2, err := New[*testBindable, *testResponse](nil, prompt,
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

func TestWithUserPromptSuffix(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	suffix, err := promptbuilder.NewPrompt("lens suffix body")
	if err != nil {
		t.Fatalf("NewPrompt(suffix) error = %v", err)
	}

	// Nil suffix: option must fail at construction.
	if _, err := New[*testBindable, *testResponse](nil, prompt,
		WithUserPromptSuffix[*testBindable, *testResponse](nil),
	); err == nil {
		t.Error("New(WithUserPromptSuffix(nil)): got = nil, want error")
	}

	// Valid suffix: stored on the executor for concatenation at Execute time.
	exec, err := New[*testBindable, *testResponse](nil, prompt,
		WithUserPromptSuffix[*testBindable, *testResponse](suffix),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	e := exec.(*executor[*testBindable, *testResponse])
	if got, want := e.userPromptSuffix, suffix; got != want {
		t.Errorf("userPromptSuffix: got = %p, want = %p", got, want)
	}

	// Default: no suffix.
	execDefault, err := New[*testBindable, *testResponse](nil, prompt)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := execDefault.(*executor[*testBindable, *testResponse]).userPromptSuffix; got != nil {
		t.Errorf("default userPromptSuffix: got = %p, want = nil", got)
	}
}

func TestWithCacheTTL(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	tests := []struct {
		name    string
		ttl     time.Duration
		wantErr bool
	}{
		{name: "valid 1 hour", ttl: time.Hour, wantErr: false},
		{name: "valid 5 minutes", ttl: 5 * time.Minute, wantErr: false},
		{name: "valid 1 minute", ttl: time.Minute, wantErr: false},
		{name: "too short 30s", ttl: 30 * time.Second, wantErr: true},
		{name: "zero", ttl: 0, wantErr: true},
		{name: "negative", ttl: -time.Minute, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New[*testBindable, *testResponse](nil, prompt,
				WithCacheTTL[*testBindable, *testResponse](tt.ttl),
			)
			if (err != nil) != tt.wantErr {
				t.Errorf("WithCacheTTL(%v) error = %v, wantErr %v", tt.ttl, err, tt.wantErr)
			}
		})
	}
}

func TestToolDeclarationsSorted(t *testing.T) {
	t.Parallel()

	// Build declarations from a map (non-deterministic order)
	tools := map[string]string{
		"zebra":  "zebra tool",
		"alpha":  "alpha tool",
		"middle": "middle tool",
		"beta":   "beta tool",
	}

	// Run multiple times to verify sorting overcomes map randomness
	for range 10 {
		defs := make([]*genai.FunctionDeclaration, 0, len(tools))
		for name, desc := range tools {
			defs = append(defs, &genai.FunctionDeclaration{
				Name:        name,
				Description: desc,
			})
		}

		// Apply the same sort the executor uses
		slices.SortFunc(defs, func(a, b *genai.FunctionDeclaration) int {
			return cmp.Compare(a.Name, b.Name)
		})

		expected := []string{"alpha", "beta", "middle", "zebra"}
		for i, def := range defs {
			if def.Name != expected[i] {
				t.Errorf("tool[%d] = %q, want %q", i, def.Name, expected[i])
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
