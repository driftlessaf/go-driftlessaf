/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
)

// Exercise the public constructors and real SDKs. Redirect only network I/O to
// a local TLS server; Google SDKs clone *http.Transport when creating clients.
// These tests must remain serial because they replace http.DefaultTransport.
func migrationHTTP(t *testing.T, handle func(*http.Request) (string, string, error)) {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
	t.Setenv("CLAUDE_BACKEND", "vertex")
	for _, key := range []string{"ANTHROPIC_PROFILE", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "OPENAI_API_KEY"} {
		t.Setenv(key, "")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, contentType := `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`, "application/json"
		if r.Host != "oauth2.googleapis.com" {
			var err error
			if strings.Contains(r.URL.Path, "/cachedContents") {
				// Cache contents are provider-owned; keep the fixture name stable so the
				// following GenerateContent requests can be compared across constructors.
				body = `{"name":"projects/test-project/locations/global/cachedContents/fixture"}`
			} else {
				body, contentType, err = handle(r)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if host != "oauth2.googleapis.com" && !strings.HasSuffix(host, "aiplatform.googleapis.com") {
				return nil, fmt.Errorf("unexpected HTTP host %q", host)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
}

func migrationRoute(protocol modelrouter.Protocol) modelrouter.Route {
	id := "gemini-2.5-flash"
	if protocol == modelrouter.ProtocolAnthropicMessages {
		id = "claude-sonnet-4-6"
	}
	if protocol == modelrouter.ProtocolOpenAIChatCompletions {
		id = "google/gemini-2.5-flash"
	}
	route := routedTestRoute(modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: id}, protocol, id)
	route.Attribution = modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: "google.vertex"}
	route.Capabilities.Efforts = []effort.Level{effort.High}
	route.Capabilities.SamplingParameters = true
	route.Capabilities.PromptCaching = true
	route.Capabilities.SuspendResume = true
	return route
}

func migrationRouter(t *testing.T, route modelrouter.Route) *Router {
	t.Helper()
	googleAdapter, err := NewVertexGoogleGenAIAdapter("test-project", "global")
	if err != nil {
		t.Fatal(err)
	}
	claudeAdapter, err := NewVertexAnthropicMessagesAdapter("test-project", "global")
	if err != nil {
		t.Fatal(err)
	}
	chatAdapter, err := NewVertexOpenAIChatCompletionsAdapter("test-project", "global")
	if err != nil {
		t.Fatal(err)
	}
	googleAdapters, err := NewGoogleGenAIAdapterRegistry(GoogleGenAIRegistration{Provider: modelrouter.ProviderVertexAI, Adapter: googleAdapter})
	if err != nil {
		t.Fatal(err)
	}
	claudeAdapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{Provider: modelrouter.ProviderVertexAI, Adapter: claudeAdapter})
	if err != nil {
		t.Fatal(err)
	}
	chatAdapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{Provider: modelrouter.ProviderVertexAI, Adapter: chatAdapter})
	if err != nil {
		t.Fatal(err)
	}
	return mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{GoogleGenAI: googleAdapters, AnthropicMessages: claudeAdapters, OpenAIChatCompletions: chatAdapters})
}

func migrationAgent(t *testing.T, routed bool, route modelrouter.Route, cfg Config[*basetenTestResponse, toolcall.EmptyTools]) Agent[*testRequest, *basetenTestResponse, toolcall.EmptyTools] {
	t.Helper()
	var agent Agent[*testRequest, *basetenTestResponse, toolcall.EmptyTools]
	var err error
	if routed {
		agent, err = NewRouted[*testRequest](t.Context(), migrationRouter(t, route), route.Selection, cfg)
	} else {
		agent, err = New[*testRequest](t.Context(), "test-project", "global", route.Selection.LogicalModel, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func migrationResponse(t *testing.T, protocol modelrouter.Protocol, turn int, name, args string) (string, string, error) {
	t.Helper()
	id := fmt.Sprintf("call-%d", turn)
	switch protocol {
	case modelrouter.ProtocolGoogleGenAI:
		return fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":%q,"args":%s}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`, name, args), "application/json", nil
	case modelrouter.ProtocolOpenAIChatCompletions:
		return fmt.Sprintf(`{"id":"completion","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`, id, name, args), "application/json", nil
	default:
		events := []string{
			`{"type":"message_start","message":{"id":"msg","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":7,"output_tokens":1}}}`,
			fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, id, name),
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, args),
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		}
		var stream strings.Builder
		for _, event := range events {
			var value struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(event), &value); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&stream, "event: %s\ndata: %s\n\n", value.Type, event)
		}
		return stream.String(), "text/event-stream", nil
	}
}

func TestNewRoutedMigrationConversationParity(t *testing.T) {
	for _, protocol := range []modelrouter.Protocol{modelrouter.ProtocolGoogleGenAI, modelrouter.ProtocolAnthropicMessages, modelrouter.ProtocolOpenAIChatCompletions} {
		t.Run(string(protocol), func(t *testing.T) {
			for _, scenario := range []string{"tool loop and rejected submission", "turn limit", "validator error", "cancellation"} {
				t.Run(scenario, func(t *testing.T) {
					route := migrationRoute(protocol)
					var legacyRequests []map[string]any
					var legacyURLs []string
					for _, routed := range []bool{false, true} {
						t.Run(fmt.Sprintf("routed=%v", routed), func(t *testing.T) {
							var requests []map[string]any
							var urls []string
							migrationHTTP(t, func(r *http.Request) (string, string, error) {
								var request map[string]any
								if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
									return "", "", err
								}
								requests = append(requests, request)
								urls = append(urls, r.URL.String())
								name, args := "lookup", `{"reasoning":"read fixture"}`
								if len(requests) > 1 {
									name, args = "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`
								}
								return migrationResponse(t, protocol, len(requests), name, args)
							})
							cfg := routedTestConfig(t)
							cfg.SystemInstructions, _ = promptbuilder.NewPrompt("review the change")
							cfg.UserPromptSuffix, _ = promptbuilder.NewPrompt("check the tests")
							cfg.Effort = effort.High
							cfg.ToolCallConcurrency = 1
							cfg.MaxTurns = 3
							tools := new(bedrockChatTools)
							cfg.Tools = tools
							validations := 0
							validationErr := errors.New("fixture validator failed")
							cfg.ResultValidators = []callbacks.ResultValidator[*basetenTestResponse]{func(context.Context, *basetenTestResponse, string) ([]callbacks.Finding, error) {
								validations++
								if scenario == "validator error" {
									return nil, validationErr
								}
								if validations == 1 {
									return []callbacks.Finding{{Kind: callbacks.FindingKindReview, Details: "please check again"}}, nil
								}
								return nil, nil
							}}
							if scenario == "turn limit" {
								cfg.MaxTurns = 1
							}
							agent := migrationAgent(t, routed, route, cfg)
							tracer := new(basetenRecordingTracer)
							ctx, cancel := context.WithCancel(agenttrace.WithTracer[*basetenTestResponse](t.Context(), tracer))
							defer cancel()
							if scenario == "cancellation" {
								cancel()
							}
							result, err := agent.Execute(ctx, &testRequest{}, toolcall.EmptyTools{})
							switch scenario {
							case "tool loop and rejected submission":
								if err != nil {
									t.Fatal(err)
								}
								if result.Answer != "done" || len(requests) != 3 || tools.calls != 1 || validations != 2 {
									t.Fatalf("result/requests/tools/validators: got = %v/%d/%d/%d, want = done/3/1/2", result, len(requests), tools.calls, validations)
								}
							case "turn limit":
								wantRequests := 1
								// Google sends a final message after its turn budget.
								// Constructor migration preserves that behavior.
								if protocol == modelrouter.ProtocolGoogleGenAI {
									wantRequests = 2
								}
								if err == nil || len(requests) != wantRequests || tools.calls != 1 {
									t.Fatalf("error/requests/tools: got = %v/%d/%d, want = error/%d/1", err, len(requests), tools.calls, wantRequests)
								}
							case "validator error":
								if !errors.Is(err, validationErr) || validations != 1 {
									t.Fatalf("error/validators: got = %v/%d, want = validator error/1", err, validations)
								}
							case "cancellation":
								if !errors.Is(err, context.Canceled) || len(requests) != 0 {
									t.Fatalf("error/requests: got = %v/%d, want = canceled/0", err, len(requests))
								}
							}
							if len(tracer.traces) != 1 {
								t.Fatalf("traces: got = %d, want = 1", len(tracer.traces))
							}
							if scenario == "tool loop and rejected submission" {
								if len(tracer.traces[0].Turns) != 3 {
									t.Fatalf("trace turns: got = %d, want = 3", len(tracer.traces[0].Turns))
								}
								for _, turn := range tracer.traces[0].Turns {
									if turn.InputTokens != 7 || turn.OutputTokens != 3 {
										t.Errorf("usage: got = %d/%d, want = 7/3", turn.InputTokens, turn.OutputTokens)
									}
								}
							}
							if routed {
								if diff := cmp.Diff(legacyRequests, requests); diff != "" {
									t.Errorf("requests (-New +NewRouted):\n%s", diff)
								}
								if diff := cmp.Diff(legacyURLs, urls); diff != "" {
									t.Errorf("endpoints (-New +NewRouted):\n%s", diff)
								}
								for _, trace := range tracer.traces {
									for _, turn := range trace.Turns {
										if turn.LogicalModel != route.Selection.LogicalModel || turn.Protocol != string(protocol) || turn.Provider != route.Attribution.ProviderName {
											t.Errorf("routed attribution: got = %q/%q/%q", turn.LogicalModel, turn.Protocol, turn.Provider)
										}
										if got, want := turn.ServingLocation, "global"; got != want {
											t.Errorf("serving location: got = %q, want = %q", got, want)
										}
									}
								}
							} else {
								legacyRequests, legacyURLs = requests, urls
							}
						})
					}
				})
			}
		})
	}
}

// A deployment can wake a checkpoint created before the constructor migration.
// Check both migration and rollback: the stored request digest must still match.
func TestNewRoutedMigrationResumesLegacyCheckpoint(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("suspend-routed=%v", routed), func(t *testing.T) {
			route := migrationRoute(modelrouter.ProtocolAnthropicMessages)
			requests := 0
			migrationHTTP(t, func(r *http.Request) (string, string, error) {
				requests++
				if requests == 1 {
					return migrationResponse(t, route.Protocol, requests, "ask_a_friend", `{"question":"ship it?"}`)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return "", "", err
				}
				if !strings.Contains(string(body), "ship the fix") || !strings.Contains(string(body), "call-1") {
					t.Error("resume request lost the answer or suspended tool-call ID")
				}
				return migrationResponse(t, route.Protocol, requests, "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`)
			})
			cfg := routedTestConfig(t)
			cfg.MaxTurns = 3
			cfg.SuspendToolName = "ask_a_friend"
			cfg.SuspendToolDescription = "Ask a human."
			cfg.UserPromptSuffix, _ = promptbuilder.NewPrompt("check the tests")
			_, err := migrationAgent(t, routed, route, cfg).Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{})
			suspension, ok := checkpoint.AsSuspension(err)
			if !ok {
				t.Fatalf("Execute: got = %v, want = suspension", err)
			}
			resumer, ok := AsResumer[*testRequest](migrationAgent(t, !routed, route, cfg))
			if !ok {
				t.Fatal("constructed agent is not a Resumer")
			}
			result, err := resumer.Resume(t.Context(), suspension.Envelope, map[string]string{"call-1": "ship the fix"}, toolcall.EmptyTools{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != "done" || requests != 2 {
				t.Fatalf("result/requests: got = %v/%d, want = done/2", result, requests)
			}
		})
	}
}

func TestNewRoutedMigrationOutputLimits(t *testing.T) {
	for _, protocol := range []modelrouter.Protocol{modelrouter.ProtocolGoogleGenAI, modelrouter.ProtocolAnthropicMessages, modelrouter.ProtocolOpenAIChatCompletions} {
		for _, configured := range []int64{0, 2048} {
			for _, routed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/max=%d/routed=%v", protocol, configured, routed), func(t *testing.T) {
					calls := 0
					migrationHTTP(t, func(r *http.Request) (string, string, error) {
						calls++
						var request map[string]any
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							return "", "", err
						}
						var got any
						var want int64
						switch protocol {
						case modelrouter.ProtocolGoogleGenAI:
							got, want = request["generationConfig"].(map[string]any)["maxOutputTokens"], 65536
						case modelrouter.ProtocolAnthropicMessages:
							got, want = request["max_tokens"], 32000
						case modelrouter.ProtocolOpenAIChatCompletions:
							got, want = request["max_completion_tokens"], 32768
						}
						if configured != 0 && (routed || protocol == modelrouter.ProtocolAnthropicMessages) {
							want = configured
						}
						if got != float64(want) {
							t.Errorf("output limit: got = %v, want = %d", got, want)
						}
						return migrationResponse(t, protocol, calls, "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`)
					})
					cfg := routedTestConfig(t)
					cfg.MaxTokens = configured
					result, err := migrationAgent(t, routed, migrationRoute(protocol), cfg).Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{})
					if err != nil {
						t.Fatal(err)
					}
					if result.Answer != "done" || calls != 1 {
						t.Fatalf("result/calls: got = %v/%d, want = done/1", result, calls)
					}
				})
			}
		}
	}
}

func TestNewRoutedMigrationRejectsUnsupportedLegacySettings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protocol  modelrouter.Protocol
		configure func(*Config[*basetenTestResponse, toolcall.EmptyTools])
	}{
		{"Chat Completions thinking budget", modelrouter.ProtocolOpenAIChatCompletions, func(c *Config[*basetenTestResponse, toolcall.EmptyTools]) { c.ThinkingBudget = 2048 }},
		{"Google refusal recovery", modelrouter.ProtocolGoogleGenAI, func(c *Config[*basetenTestResponse, toolcall.EmptyTools]) { c.RefusalNudgeMaxRetries = 1 }},
		{"Chat Completions refusal recovery", modelrouter.ProtocolOpenAIChatCompletions, func(c *Config[*basetenTestResponse, toolcall.EmptyTools]) { c.RefusalNudgeMaxRetries = 1 }},
		{"Google suspend", modelrouter.ProtocolGoogleGenAI, func(c *Config[*basetenTestResponse, toolcall.EmptyTools]) { c.SuspendToolName = "ask_a_friend" }},
		{"Chat Completions suspend", modelrouter.ProtocolOpenAIChatCompletions, func(c *Config[*basetenTestResponse, toolcall.EmptyTools]) { c.SuspendToolName = "ask_a_friend" }},
		{"undeclared effort", modelrouter.ProtocolAnthropicMessages, func(c *Config[*basetenTestResponse, toolcall.EmptyTools]) { c.Effort = effort.XHigh }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := migrationRoute(tc.protocol)
			// No adapters: a capability error must win over missing-adapter or auth
			// failures, so the caller learns which setting needs migration first.
			router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{})
			cfg := routedTestConfig(t)
			tc.configure(&cfg)
			if _, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg); !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
				t.Fatalf("NewRouted: got = %v, want = ErrUnsupportedCapability", err)
			}
		})
	}
}

func TestNewRoutedRejectsSuspendSubmitCollisionBeforeAdapter(t *testing.T) {
	route := migrationRoute(modelrouter.ProtocolAnthropicMessages)
	router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{})
	cfg := routedTestConfig(t)
	cfg.SuspendToolName = "submit_result"
	if _, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg); err == nil || !strings.Contains(err.Error(), "collides with the submit tool name") {
		t.Fatalf("NewRouted: got = %v, want = suspend/submit collision before adapter lookup", err)
	}
}
