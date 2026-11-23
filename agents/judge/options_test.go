/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

// allEfforts is the full provider-neutral scale, for routes that declare
// every level.
var allEfforts = []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}

// geminiThinking is the part of a Gemini request's thinkingConfig that an
// effort sets.
type geminiThinking struct {
	ThinkingLevel  string `json:"thinkingLevel"`
	ThinkingBudget int32  `json:"thinkingBudget"`
}

// geminiGeneration is the part of a Gemini request's generationConfig that
// an effort changes: the thinking control and the output cap.
type geminiGeneration struct {
	Thinking        *geminiThinking
	MaxOutputTokens int32
}

// claudeOutputConfig is the part of a Messages request's output_config that
// an effort sets.
type claudeOutputConfig struct {
	Effort string `json:"effort"`
}

// geminiBuilds are the construction paths that accept Option on the Gemini
// backend. The NewVertex path runs newVertexWithLabels with the test client
// in place of the Vertex client NewVertex would create.
var geminiBuilds = []struct {
	name  string
	build func(t *testing.T, client *genai.Client, model string, opts []Option) (Interface, error)
}{{
	name: "NewVertex",
	build: func(t *testing.T, client *genai.Client, model string, opts []Option) (Interface, error) {
		return newTestVertexGeminiJudge(t, client, model, anyOptions(opts)...)
	},
}, {
	name: "NewRouted",
	build: func(t *testing.T, client *genai.Client, model string, opts []Option) (Interface, error) {
		selection := modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: model}
		route := judgeTestRoute(selection, modelrouter.ProtocolGoogleGenAI, model)
		route.Capabilities.Efforts = allEfforts
		router := mustJudgeRouter(t, []modelrouter.Route{route}, metaagent.AdapterRegistries{
			GoogleGenAI: mustGoogleJudgeAdapters(t, metaagent.GoogleGenAIRegistration{
				Provider: selection.Provider,
				Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.GoogleGenAIBinding, error) {
					return metaagent.NewGoogleGenAIBinding(plan, client, nil)
				},
			}),
		})
		return NewRouted(t.Context(), router, selection, opts...)
	},
}}

// claudeBuilds are the construction paths that accept Option on the Claude
// backend. The NewVertex path runs newVertexWithLabels with the test client
// in place of the Messages client NewVertex would resolve.
var claudeBuilds = []struct {
	name  string
	build func(t *testing.T, messages anthropic.MessageService, model string, opts []Option) (Interface, error)
}{{
	name: "NewVertex",
	build: func(t *testing.T, messages anthropic.MessageService, model string, opts []Option) (Interface, error) {
		return newTestVertexClaudeJudge(t, messages, model, anyOptions(opts)...)
	},
}, {
	name: "NewRouted",
	build: func(t *testing.T, messages anthropic.MessageService, model string, opts []Option) (Interface, error) {
		selection := modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: model}
		route := judgeTestRoute(selection, modelrouter.ProtocolAnthropicMessages, model)
		route.Capabilities.Efforts = allEfforts
		router := mustJudgeRouter(t, []modelrouter.Route{route}, metaagent.AdapterRegistries{
			AnthropicMessages: mustAnthropicJudgeAdapters(t, metaagent.AnthropicMessagesRegistration{
				Provider: selection.Provider,
				Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
					return metaagent.NewAnthropicMessagesBinding(plan, messages, nil)
				},
			}),
		})
		return NewRouted(t.Context(), router, selection, opts...)
	},
}}

// The effort reaches each Gemini request as the thinking control the model
// takes, on every construction path. Without an effort, the request carries
// no thinking configuration at all, so the model keeps its default.
func TestWithEffortReachesGeminiRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		model string
		opts  []Option
		want  *geminiThinking
	}{{
		name:  "no option sends no thinking config",
		model: "gemini-4-hydrogen",
	}, {
		name:  "an empty level sends no thinking config",
		model: "gemini-4-hydrogen",
		opts:  []Option{WithEffort("")},
	}, {
		name:  "Gemini 4 takes low as the LOW thinking level",
		model: "gemini-4-hydrogen",
		opts:  []Option{WithEffort(effort.Low)},
		want:  &geminiThinking{ThinkingLevel: "LOW"},
	}, {
		name:  "Gemini 4 takes medium as the MEDIUM thinking level",
		model: "gemini-4-neon",
		opts:  []Option{WithEffort(effort.Medium)},
		want:  &geminiThinking{ThinkingLevel: "MEDIUM"},
	}, {
		name:  "Gemini 2.5 takes low as a thinking budget",
		model: "gemini-2.5-flash",
		opts:  []Option{WithEffort(effort.Low)},
		want:  &geminiThinking{ThinkingBudget: 1024},
	}} {
		for _, path := range geminiBuilds {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				client, generation := newGeminiJudgeServer(t)
				judgeInstance, err := path.build(t, client, tc.model, tc.opts)
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
					t.Fatalf("Judge: %v", err)
				}
				if diff := cmp.Diff(tc.want, (<-generation).Thinking); diff != "" {
					t.Errorf("thinkingConfig (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// The effort reaches each Messages request as output_config.effort, on every
// construction path. Without an effort, the request carries no
// output_config at all, so the model keeps its default.
func TestWithEffortReachesClaudeRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		model string
		opts  []Option
		want  *claudeOutputConfig
	}{{
		name:  "no option sends no output config",
		model: "claude-haiku-5-5",
	}, {
		name:  "an empty level sends no output config",
		model: "claude-haiku-5-5",
		opts:  []Option{WithEffort("")},
	}, {
		name:  "low is sent as is",
		model: "claude-haiku-5-5",
		opts:  []Option{WithEffort(effort.Low)},
		want:  &claudeOutputConfig{Effort: "low"},
	}, {
		name:  "max is sent as is on a model that predates xhigh",
		model: "claude-sonnet-4-6",
		opts:  []Option{WithEffort(effort.Max)},
		want:  &claudeOutputConfig{Effort: "max"},
	}} {
		for _, path := range claudeBuilds {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				messages, outputConfig := newClaudeJudgeServer(t)
				judgeInstance, err := path.build(t, messages, tc.model, tc.opts)
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
					t.Fatalf("Judge: %v", err)
				}
				if diff := cmp.Diff(tc.want, <-outputConfig); diff != "" {
					t.Errorf("output_config (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// Gemini counts thinking tokens against max_output_tokens. When the effort
// maps to a thinking budget, the judge raises its cap by that budget on every
// construction path, so medium, xhigh and max build on those models and the
// judgement keeps the room it had without an effort.
func TestWithEffortRaisesTheGeminiCapByTheThinkingBudget(t *testing.T) {
	t.Parallel()
	const base = int32(defaultMaxTokens)
	for _, tc := range []struct {
		name  string
		model string
		level effort.Level
		want  geminiGeneration
	}{{
		name:  "no effort keeps the default cap",
		model: "gemini-2.5-flash",
		want:  geminiGeneration{MaxOutputTokens: base},
	}, {
		name:  "low adds its budget",
		model: "gemini-2.5-flash",
		level: effort.Low,
		want:  geminiGeneration{Thinking: &geminiThinking{ThinkingBudget: 1024}, MaxOutputTokens: base + 1024},
	}, {
		name:  "medium adds its budget",
		model: "gemini-2.5-flash",
		level: effort.Medium,
		want:  geminiGeneration{Thinking: &geminiThinking{ThinkingBudget: 8192}, MaxOutputTokens: base + 8192},
	}, {
		name:  "max adds its budget",
		model: "gemini-2.5-flash",
		level: effort.Max,
		want:  geminiGeneration{Thinking: &geminiThinking{ThinkingBudget: 24576}, MaxOutputTokens: base + 24576},
	}, {
		name:  "high thinks dynamically and keeps the default cap",
		model: "gemini-2.5-flash",
		level: effort.High,
		want:  geminiGeneration{Thinking: &geminiThinking{ThinkingBudget: -1}, MaxOutputTokens: base},
	}, {
		name:  "a thinking-level model keeps the default cap",
		model: "gemini-4-neon",
		level: effort.Medium,
		want:  geminiGeneration{Thinking: &geminiThinking{ThinkingLevel: "MEDIUM"}, MaxOutputTokens: base},
	}} {
		for _, path := range geminiBuilds {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var opts []Option
				if tc.level != "" {
					opts = []Option{WithEffort(tc.level)}
				}
				client, generation := newGeminiJudgeServer(t)
				judgeInstance, err := path.build(t, client, tc.model, opts)
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
					t.Fatalf("Judge: %v", err)
				}
				if diff := cmp.Diff(tc.want, <-generation); diff != "" {
					t.Errorf("generationConfig (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// NewVertex places the judge's own executor options ahead of the caller's,
// so an executor option the caller passes wins: an executor WithEffort over
// WithEffort, and an executor WithMaxOutputTokens over the cap the judge
// raises for a thinking budget.
func TestNewVertexExecutorOptionsOverrideTheJudgeEffort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		model string
		opts  []any
		want  geminiGeneration
	}{{
		name:  "an executor effort wins over the judge effort",
		model: "gemini-4-hydrogen",
		opts:  []any{WithEffort(effort.Low), googleexecutor.WithEffort[*Request, *Judgement](effort.Medium)},
		want:  geminiGeneration{Thinking: &geminiThinking{ThinkingLevel: "MEDIUM"}, MaxOutputTokens: int32(defaultMaxTokens)},
	}, {
		name:  "an executor cap wins over the raised cap",
		model: "gemini-2.5-flash",
		opts:  []any{WithEffort(effort.Medium), googleexecutor.WithMaxOutputTokens[*Request, *Judgement](12000)},
		want:  geminiGeneration{Thinking: &geminiThinking{ThinkingBudget: 8192}, MaxOutputTokens: 12000},
	}} {
		t.Run("Gemini/"+tc.name, func(t *testing.T) {
			t.Parallel()
			client, generation := newGeminiJudgeServer(t)
			judgeInstance, err := newTestVertexGeminiJudge(t, client, tc.model, tc.opts...)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if diff := cmp.Diff(tc.want, <-generation); diff != "" {
				t.Errorf("generationConfig (-want +got):\n%s", diff)
			}
		})
	}
	t.Run("Claude/an executor effort wins over the judge effort", func(t *testing.T) {
		t.Parallel()
		messages, outputConfig := newClaudeJudgeServer(t)
		judgeInstance, err := newTestVertexClaudeJudge(t, messages, "claude-haiku-5-5",
			WithEffort(effort.Low), claudeexecutor.WithEffort[*Request, *Judgement](effort.Medium))
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
			t.Fatalf("Judge: %v", err)
		}
		if diff := cmp.Diff(&claudeOutputConfig{Effort: "medium"}, <-outputConfig); diff != "" {
			t.Errorf("output_config (-want +got):\n%s", diff)
		}
	})
}

// A level outside the shared scale fails construction on every path, so it
// never reaches a provider.
func TestWithEffortRejectsAnInvalidLevel(t *testing.T) {
	t.Parallel()
	opts := []Option{WithEffort("extreme")}
	const wantErr = `invalid effort level "extreme"`
	for _, path := range geminiBuilds {
		t.Run("Gemini/"+path.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newGeminiJudgeServer(t)
			if _, err := path.build(t, client, "gemini-4-hydrogen", opts); err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("build error: got = %v, want it to contain %q", err, wantErr)
			}
		})
	}
	for _, path := range claudeBuilds {
		t.Run("Claude/"+path.name, func(t *testing.T) {
			t.Parallel()
			messages, _ := newClaudeJudgeServer(t)
			if _, err := path.build(t, messages, "claude-haiku-5-5", opts); err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("build error: got = %v, want it to contain %q", err, wantErr)
			}
		})
	}
}

// A routed judge refuses an effort its route does not serve before it binds
// the adapter, instead of dropping the effort or sending one the provider
// rejects.
func TestNewRoutedRejectsAnEffortTheRouteDoesNotServe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		model   string
		efforts []effort.Level
		level   effort.Level
	}{{
		name:  "the route declares no effort",
		model: "claude-haiku-5-5",
		level: effort.Low,
	}, {
		name:    "the model predates xhigh",
		model:   "claude-sonnet-4-6",
		efforts: allEfforts,
		level:   effort.XHigh,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			selection := modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: tc.model}
			route := judgeTestRoute(selection, modelrouter.ProtocolAnthropicMessages, tc.model)
			route.Capabilities.Efforts = tc.efforts
			adapterCalls := 0
			router := mustJudgeRouter(t, []modelrouter.Route{route}, metaagent.AdapterRegistries{
				AnthropicMessages: mustAnthropicJudgeAdapters(t, metaagent.AnthropicMessagesRegistration{
					Provider: selection.Provider,
					Adapter: func(context.Context, modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
						adapterCalls++
						return metaagent.AnthropicMessagesBinding{}, errors.New("must not be called")
					},
				}),
			})
			_, err := NewRouted(t.Context(), router, selection, WithEffort(tc.level))
			if !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
				t.Errorf("NewRouted error: got = %v, want ErrUnsupportedCapability", err)
			}
			if adapterCalls != 0 {
				t.Errorf("adapter calls: got = %d, want = 0", adapterCalls)
			}
		})
	}
}

// newTestVertexGeminiJudge runs newVertexWithLabels with the test client in
// place of the Vertex client NewVertex would create.
func newTestVertexGeminiJudge(t *testing.T, client *genai.Client, model string, opts ...any) (Interface, error) {
	t.Helper()
	return newVertexWithLabels(t.Context(), "project-id", "global", model, nil, legacyJudgeConstructors{
		google: func(_ context.Context, _, _, modelName string, _ map[string]string, execOpts ...googleexecutor.Option[*Request, *Judgement]) (Interface, error) {
			return newGoogleWithClient(googleJudgeConstruction{
				client:          client,
				providerModelID: modelName,
				logicalModelID:  modelName,
				samplingParams:  true,
			}, execOpts...)
		},
	}, opts...)
}

// newTestVertexClaudeJudge runs newVertexWithLabels with the test client in
// place of the Messages client NewVertex would resolve.
func newTestVertexClaudeJudge(t *testing.T, messages anthropic.MessageService, model string, opts ...any) (Interface, error) {
	t.Helper()
	return newVertexWithLabels(t.Context(), "project-id", "global", model, nil, legacyJudgeConstructors{
		claude: func(_ context.Context, _, _, modelName string, _ map[string]string, execOpts ...claudeexecutor.Option[*Request, *Judgement]) (Interface, error) {
			provider := claudeexecutor.ProviderAnthropic
			return newClaudeWithMessages(claudeJudgeConstruction{
				messages:        messages,
				providerModelID: modelName,
				logicalModelID:  modelName,
				legacyProvider:  &provider,
				samplingParams:  true,
			}, execOpts...)
		},
	}, opts...)
}

func anyOptions(opts []Option) []any {
	out := make([]any, 0, len(opts))
	for _, opt := range opts {
		out = append(out, opt)
	}
	return out
}

// newGeminiJudgeServer returns a client for a fake Vertex endpoint. The
// endpoint answers one request with a judgement and sends that request's
// thinkingConfig, nil when the request has none, and maxOutputTokens on the
// returned channel.
func newGeminiJudgeServer(t *testing.T) (*genai.Client, <-chan geminiGeneration) {
	t.Helper()
	generation := make(chan geminiGeneration, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload struct {
			GenerationConfig struct {
				ThinkingConfig  *geminiThinking `json:"thinkingConfig"`
				MaxOutputTokens int32           `json:"maxOutputTokens"`
			} `json:"generationConfig"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		select {
		case generation <- geminiGeneration{Thinking: payload.GenerationConfig.ThinkingConfig, MaxOutputTokens: payload.GenerationConfig.MaxOutputTokens}:
		default:
			t.Errorf("unexpected second request to %s", request.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"finishReason":"STOP"}]}`, judgementJSON(StandaloneMode))
	}))
	t.Cleanup(server.Close)
	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     "effort-test-project",
		Location:    "global",
		HTTPClient:  server.Client(),
		HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	return client, generation
}

// newClaudeJudgeServer returns a Messages client for a fake endpoint. The
// endpoint answers one request with a judgement and sends that request's
// output_config on the returned channel, nil when the request has none.
func newClaudeJudgeServer(t *testing.T) (anthropic.MessageService, <-chan *claudeOutputConfig) {
	t.Helper()
	outputConfig := make(chan *claudeOutputConfig, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload struct {
			Model        string              `json:"model"`
			OutputConfig *claudeOutputConfig `json:"output_config"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		select {
		case outputConfig <- payload.OutputConfig:
		default:
			t.Errorf("unexpected second request to %s", request.URL.Path)
		}
		writeAnthropicJudgement(w, payload.Model, StandaloneMode)
	}))
	t.Cleanup(server.Close)
	return anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey("effort-test-token"),
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0),
	).Messages, outputConfig
}
