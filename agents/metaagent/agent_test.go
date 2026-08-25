/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

type testRequest struct{}

func (r *testRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type testResponse struct{}

type basetenTestResponse struct {
	Answer string `json:"answer"`
}

type basetenRecordingTracer struct {
	traces []*agenttrace.Trace[*basetenTestResponse]
}

func (r *basetenRecordingTracer) NewTrace(ctx context.Context, prompt string, opts ...agenttrace.StartTraceOption) *agenttrace.Trace[*basetenTestResponse] {
	return agenttrace.NewDefaultTracer[*basetenTestResponse](ctx).NewTrace(ctx, prompt, opts...)
}

func (r *basetenRecordingTracer) RecordTrace(trace *agenttrace.Trace[*basetenTestResponse]) {
	r.traces = append(r.traces, trace)
}

type failingToolsProvider struct{}

func (failingToolsProvider) Tools(context.Context, toolcall.EmptyTools) (map[string]toolcall.Tool[*testResponse], error) {
	return nil, errors.New("building test tools")
}

// testCallbacks is the standard tool composition: Empty -> Worktree -> Finding
type testCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

func TestNewModelSelection(t *testing.T) {
	config := Config[*testResponse, testCallbacks]{
		Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
			toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
				toolcall.NewEmptyToolsProvider[*testResponse]())),
	}

	tests := []struct {
		name    string
		model   string
		wantErr string
	}{{
		name:    "unsupported model",
		model:   "unknown-model",
		wantErr: "unsupported model",
	}, {
		name:    "empty model",
		model:   "",
		wantErr: "unsupported model",
	}, {
		name:    "partial gemini",
		model:   "gem",
		wantErr: "unsupported model",
	}, {
		name:    "partial claude",
		model:   "cla",
		wantErr: "unsupported model",
	}, {
		name:    "slash routes to openai compat",
		model:   "google/gemini-2.5-pro",
		wantErr: "prompt cannot be nil",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New[*testRequest](t.Context(), "test-project", "us-central1", tt.model, config)
			if err == nil {
				t.Errorf("New() got = nil, want error")
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New() got = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestEffortWiredOnGoogleAndOpenAIBackends pins the cross-backend Effort
// contract: Config.Effort must reach the Gemini and OpenAI-compatible
// backends' executor options rather than being silently dropped. A valid
// level must leave construction succeeding, and an invalid level must fail
// construction on both backends — the rejection is what proves the wiring
// exists, since removing the Effort plumbing from a backend would make the
// invalid case construct successfully again.
//
// Construction acquires Application Default Credentials before applying
// executor options, so the test points GOOGLE_APPLICATION_CREDENTIALS at a
// hermetic fake service-account key (see fakeGoogleCredentials); construction
// genuinely reaches option application rather than failing early and passing
// vacuously.
func TestEffortWiredOnGoogleAndOpenAIBackends(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))

	userPrompt, err := promptbuilder.NewPrompt("payload")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}

	// gemini-* routes to the Gemini backend; publisher/model routes to the
	// OpenAI-compatible backend.
	for _, model := range []string{"gemini-2.5-flash", "google/gemini-2.5-pro"} {
		t.Run(model, func(t *testing.T) {
			config := Config[*testResponse, testCallbacks]{
				UserPrompt: userPrompt,
				Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
					toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
						toolcall.NewEmptyToolsProvider[*testResponse]())),
			}

			config.Effort = effort.XHigh
			if _, err := New[*testRequest](t.Context(), "test-project", "us-central1", model, config); err != nil {
				t.Errorf("New() with effort %q: got = %v, want = nil", config.Effort, err)
			}

			config.Effort = "not-a-level"
			if _, err := New[*testRequest](t.Context(), "test-project", "us-central1", model, config); err == nil {
				t.Errorf("New() with effort %q: got = nil, want validation error (Effort must be wired to this backend)", config.Effort)
			}
		})
	}
}

// TestUserPromptSuffixAcceptedOnOpenAIBackend pins the config-compat contract
// of the OpenAI-compatible path: the suffix is folded into the built user
// prompt (see openaiexecutor.WithUserPromptSuffix), so setting one must not
// change whether construction succeeds. The model is operator-configurable
// (e.g. REVIEWER_MODEL), so a suffix-specific rejection here would break
// deployments pointing at a publisher/model value that worked before.
//
// Construction acquires Application Default Credentials before applying
// executor options, so the test points GOOGLE_APPLICATION_CREDENTIALS at a
// hermetic fake service-account key. That makes credential acquisition
// succeed deterministically — in CI and locally alike — and construction
// genuinely reaches option application with the suffix set, rather than
// failing early and passing this test vacuously.
func TestUserPromptSuffixAcceptedOnOpenAIBackend(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))

	userPrompt, err := promptbuilder.NewPrompt("payload")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	suffix, err := promptbuilder.NewPrompt("lens suffix")
	if err != nil {
		t.Fatalf("NewPrompt(suffix) error = %v", err)
	}

	config := Config[*testResponse, testCallbacks]{
		UserPrompt: userPrompt,
		Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
			toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
				toolcall.NewEmptyToolsProvider[*testResponse]())),
	}

	for _, region := range []string{"us-central1", "global"} {
		config.UserPromptSuffix = nil
		if _, err := New[*testRequest](t.Context(), "test-project", region, "google/gemini-2.5-pro", config); err != nil {
			t.Fatalf("New() without suffix in %s: got = %v, want = nil", region, err)
		}

		config.UserPromptSuffix = suffix
		if _, err := New[*testRequest](t.Context(), "test-project", region, "google/gemini-2.5-pro", config); err != nil {
			t.Errorf("New() with suffix in %s: got = %v, want = nil", region, err)
		}
	}
}

func TestNewOpenAICompatibleBaseten(t *testing.T) {
	const apiKey = "temporary-baseten-test-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/chat/completions"; got != want {
			t.Errorf("request path: got = %q, want = %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+apiKey; got != want {
			t.Errorf("Authorization: got = %q, want = %q", got, want)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request", http.StatusBadRequest)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "decoding request", http.StatusBadRequest)
			return
		}
		if got, ok := request["max_tokens"]; !ok || got != float64(32768) {
			t.Errorf("max_tokens: got = %v, want = 32768", got)
		}
		if _, ok := request["max_completion_tokens"]; ok {
			t.Error("request contains max_completion_tokens, want only max_tokens")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-baseten-test",
			"object":  "chat.completion",
			"created": 1,
			"model":   "openai/gpt-oss-120b",
			"choices": []any{map[string]any{
				"index":         0,
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{map[string]any{
						"id":   "call-submit",
						"type": "function",
						"function": map[string]any{
							"name":      "submit_result",
							"arguments": `{"reasoning":"complete","result":{"answer":"done"}}`,
						},
					}},
				},
			}},
			"usage": map[string]any{
				"prompt_tokens":     4,
				"completion_tokens": 2,
				"total_tokens":      6,
			},
		})
	}))
	t.Cleanup(srv.Close)

	prompt, err := promptbuilder.NewPrompt("review this change")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	config := Config[*basetenTestResponse, toolcall.EmptyTools]{
		SystemInstructions:  prompt,
		UserPrompt:          prompt,
		Tools:               toolcall.NewEmptyToolsProvider[*basetenTestResponse](),
		MaxTurns:            1,
		ToolCallConcurrency: 1,
		ResultValidators: []callbacks.ResultValidator[*basetenTestResponse]{
			func(context.Context, *basetenTestResponse, string) ([]callbacks.Finding, error) { return nil, nil },
		},
	}
	agent, err := NewOpenAICompatible[*testRequest](OpenAICompatibleProvider{
		BaseURL:             srv.URL + "/v1",
		APIKey:              apiKey,
		Provider:            openaiexecutor.ProviderBaseten,
		TokenLimitParameter: openaiexecutor.TokenLimitMaxTokens,
	}, "openai/gpt-oss-120b", config)
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}

	tracer := &basetenRecordingTracer{}
	ctx := agenttrace.WithTracer[*basetenTestResponse](t.Context(), tracer)
	response, err := agent.Execute(ctx, &testRequest{}, toolcall.EmptyTools{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := response.Answer, "done"; got != want {
		t.Errorf("response.Answer: got = %q, want = %q", got, want)
	}
	if got, want := len(tracer.traces), 1; got != want {
		t.Fatalf("recorded traces: got = %d, want = %d", got, want)
	}
	if got, want := len(tracer.traces[0].Turns), 1; got != want {
		t.Fatalf("trace turns: got = %d, want = %d", got, want)
	}
	if got, want := tracer.traces[0].Turns[0].System, agenttrace.SystemBaseten; got != want {
		t.Errorf("trace system: got = %q, want = %q", got, want)
	}
}

func TestNewOpenAICompatibleValidatesExplicitConfiguration(t *testing.T) {
	prompt, err := promptbuilder.NewPrompt("review this change")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	config := Config[*basetenTestResponse, toolcall.EmptyTools]{
		UserPrompt: prompt,
		Tools:      toolcall.NewEmptyToolsProvider[*basetenTestResponse](),
	}
	valid := OpenAICompatibleProvider{
		BaseURL:             "https://example.invalid/v1",
		APIKey:              "secret-value-that-must-not-leak",
		Provider:            openaiexecutor.ProviderBaseten,
		TokenLimitParameter: openaiexecutor.TokenLimitMaxTokens,
	}

	tests := []struct {
		name     string
		provider OpenAICompatibleProvider
		model    string
	}{
		{name: "empty base URL", provider: OpenAICompatibleProvider{APIKey: valid.APIKey}, model: "model"},
		{name: "empty API key", provider: OpenAICompatibleProvider{BaseURL: valid.BaseURL}, model: "model"},
		{name: "empty model", provider: valid},
		{name: "unknown provider", provider: OpenAICompatibleProvider{BaseURL: valid.BaseURL, APIKey: valid.APIKey, Provider: openaiexecutor.Provider("unknown"), TokenLimitParameter: valid.TokenLimitParameter}, model: "model"},
		{name: "unknown token parameter", provider: OpenAICompatibleProvider{BaseURL: valid.BaseURL, APIKey: valid.APIKey, Provider: valid.Provider, TokenLimitParameter: openaiexecutor.TokenLimitParameter("unknown")}, model: "model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewOpenAICompatible[*testRequest](tt.provider, tt.model, config)
			if err == nil {
				t.Fatal("NewOpenAICompatible: got nil error, want validation error")
			}
			if strings.Contains(err.Error(), valid.APIKey) {
				t.Errorf("error contains API key: %v", err)
			}
		})
	}
}

func TestAgentWrappersPropagateToolProviderErrors(t *testing.T) {
	config := Config[*testResponse, toolcall.EmptyTools]{Tools: failingToolsProvider{}}
	assertBuildError := func(t *testing.T, execute func() error) {
		t.Helper()
		if err := execute(); err == nil || !strings.Contains(err.Error(), "building tools: building test tools") {
			t.Errorf("agent error: got = %v, want wrapped tool-provider error", err)
		}
	}

	t.Run("OpenAI compatible", func(t *testing.T) {
		agent := &openAICompatAgent[*testRequest, *testResponse, toolcall.EmptyTools]{config: config}
		assertBuildError(t, func() error {
			_, err := agent.Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{})
			return err
		})
	})
	t.Run("Google", func(t *testing.T) {
		agent := &googleAgent[*testRequest, *testResponse, toolcall.EmptyTools]{config: config}
		assertBuildError(t, func() error {
			_, err := agent.Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{})
			return err
		})
	})
	t.Run("Claude execute", func(t *testing.T) {
		agent := &claudeAgent[*testRequest, *testResponse, toolcall.EmptyTools]{config: config}
		assertBuildError(t, func() error {
			_, err := agent.Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{})
			return err
		})
	})
	t.Run("Claude resume", func(t *testing.T) {
		agent := &claudeAgent[*testRequest, *testResponse, toolcall.EmptyTools]{config: config}
		assertBuildError(t, func() error {
			_, err := agent.Resume(t.Context(), checkpoint.Envelope{}, nil, toolcall.EmptyTools{})
			return err
		})
	})
}

// fakeGoogleCredentials writes a syntactically valid service-account key with
// a freshly generated throwaway RSA key to a temp file and returns its path.
// Application Default Credentials parses the file without any network calls,
// so pointing GOOGLE_APPLICATION_CREDENTIALS at it makes credential
// acquisition hermetic.
func fakeGoogleCredentials(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating throwaway key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	creds, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": "fake-key-id",
		"private_key":    string(keyPEM),
		"client_email":   "fake@test-project.iam.gserviceaccount.com",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshaling fake credentials: %v", err)
	}

	path := filepath.Join(t.TempDir(), "fake-credentials.json")
	if err := os.WriteFile(path, creds, 0o600); err != nil {
		t.Fatalf("writing fake credentials: %v", err)
	}
	return path
}
