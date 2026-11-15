/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	configuredModel = "claude-sonnet-5-5"
	fallbackModel   = "claude-opus-5-5"
)

// scriptedTurns plays script, one entry per request: r is a refused turn, l a
// lookup tool call, a the final answer.
func scriptedTurns(t *testing.T, name string, script string) func(n int) []string {
	t.Helper()
	return func(n int) []string {
		if n > len(script) {
			t.Errorf("%s request %d: the script has %d turns", name, n, len(script))
			return answerTurn
		}
		switch script[n-1] {
		case 'r':
			return refusalTurn
		case 'l':
			return lookupTurn(n)
		default:
			return answerTurn
		}
	}
}

func lookupTools() map[string]claudetool.Metadata[errCapResponse] {
	return map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"result": "synthetic evidence"}
			},
		},
	}
}

// requestModel is the model a request body names.
func requestModel(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decoding a request: %v", err)
	}
	return req.Model
}

// withoutModel is a request body with its model left out.
func withoutModel(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decoding a request: %v", err)
	}
	delete(req, "model")
	return req
}

// TestRefusalFallback pins WithRefusalFallback: the first refused turn is sent
// again, unchanged, to the fallback model, which serves the rest of the call;
// a refusal there is nudged under WithRefusalNudge and fatal without it.
func TestRefusalFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string // the configured model's script
		fallback   string // the fallback's script
		nudges     int
		wantAnswer bool
		// wantNudged is the fallback request, 1-based, that carries the
		// placeholder and the nudge; 0 for none.
		wantNudged int
	}{
		{name: "the fallback serves the rest of the call", configured: "r", fallback: "la", wantAnswer: true},
		{name: "a refusal on the fallback is nudged", configured: "r", fallback: "ra", nudges: 1, wantAnswer: true, wantNudged: 2},
		{name: "a refusal on the fallback without a nudge fails the run", configured: "r", fallback: "r"},
		{name: "a call that is not refused stays on the configured model", configured: "la"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, configuredRequests := newExecTestClient(t, scriptedTurns(t, "configured", tc.configured))
			fallbackClient, fallbackRequests := newExecTestClient(t, scriptedTurns(t, "fallback", tc.fallback))
			prompt, err := promptbuilder.NewPrompt("hello")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			opts := []claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithModel[errCapRequest, errCapResponse](configuredModel),
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](10),
				claudeexecutor.WithRefusalFallback[errCapRequest, errCapResponse](claudeexecutor.RefusalFallback{
					Messages:        fallbackClient.Messages,
					ProviderModelID: fallbackModel,
				}),
			}
			if tc.nudges > 0 {
				opts = append(opts, claudeexecutor.WithRefusalNudge[errCapRequest, errCapResponse](tc.nudges))
			}
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tracer := &recordingTracer{}

			resp, execErr := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, lookupTools())

			if tc.wantAnswer || tc.fallback == "" {
				if execErr != nil || resp.Answer != "42" {
					t.Fatalf("Execute: resp = %+v, err = %v, want the answer", resp, execErr)
				}
			} else if _, ok := errors.AsType[*claudeexecutor.RefusalError](execErr); !ok {
				t.Fatalf("Execute error: got %v (%T), want *claudeexecutor.RefusalError", execErr, execErr)
			}

			configured, fallback := configuredRequests(), fallbackRequests()
			if got, want := len(configured), len(tc.configured); got != want {
				t.Errorf("requests to the configured model: got = %d, want = %d", got, want)
			}
			if got, want := len(fallback), len(tc.fallback); got != want {
				t.Fatalf("requests to the fallback: got = %d, want = %d", got, want)
			}
			for i, body := range configured {
				if got := requestModel(t, body); got != configuredModel {
					t.Errorf("configured request %d model: got = %q, want = %q", i+1, got, configuredModel)
				}
			}
			for i, body := range fallback {
				if got := requestModel(t, body); got != fallbackModel {
					t.Errorf("fallback request %d model: got = %q, want = %q", i+1, got, fallbackModel)
				}
			}
			if len(fallback) > 0 {
				// The retry is the refused request itself: no placeholder, no
				// nudge, nothing but the model changed.
				refused := configured[len(configured)-1]
				if diff := cmp.Diff(withoutModel(t, refused), withoutModel(t, fallback[0])); diff != "" {
					t.Errorf("the fallback's first request is not the refused one (-refused, +fallback):\n%s", diff)
				}
			}
			for i, body := range fallback {
				nudged := strings.Contains(string(body), "response withheld by Anthropic's safety classifier")
				if want := i+1 == tc.wantNudged; nudged != want {
					t.Errorf("fallback request %d carries the nudge: got = %t, want = %t", i+1, nudged, want)
				}
			}

			if len(tracer.traces) != 1 {
				t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
			}
			turns := tracer.traces[0].Turns
			if got, want := len(turns), len(tc.configured)+len(tc.fallback); got != want {
				t.Fatalf("trace turns: got = %d, want = %d", got, want)
			}
			for i, turn := range turns {
				want := configuredModel
				if i >= len(tc.configured) {
					want = fallbackModel
				}
				if turn.Model != want {
					t.Errorf("trace turn %d model: got = %q, want = %q", i, turn.Model, want)
				}
			}
			if len(tc.fallback) > 0 {
				refused := turns[len(tc.configured)-1]
				if refused.Failed || len(refused.Errors) != 1 || !strings.Contains(refused.Errors[0], "category: cyber") || !strings.Contains(refused.Errors[0], "retried on "+fallbackModel) {
					t.Errorf("the refused turn: got failed = %t, errors = %q, want the refusal and its fallback recorded on a turn that did not fail", refused.Failed, refused.Errors)
				}
			}
		})
	}
}

// TestRefusalFallbackOnFinalTurnReturnsTypedError: a refusal on the call's
// last turn has no turn left to fall back on, so it fails as a *RefusalError
// without a request to the fallback.
func TestRefusalFallbackOnFinalTurnReturnsTypedError(t *testing.T) {
	client, _ := newExecTestClient(t, scriptedTurns(t, "configured", "r"))
	fallbackClient, fallbackRequests := newExecTestClient(t, scriptedTurns(t, "fallback", ""))
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
		claudeexecutor.WithModel[errCapRequest, errCapResponse](configuredModel),
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
		claudeexecutor.WithRefusalFallback[errCapRequest, errCapResponse](claudeexecutor.RefusalFallback{
			Messages:        fallbackClient.Messages,
			ProviderModelID: fallbackModel,
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := exec.Execute(t.Context(), errCapRequest{}, nil)

	if _, ok := errors.AsType[*claudeexecutor.RefusalError](execErr); !ok {
		t.Fatalf("Execute error: got %v (%T), want *claudeexecutor.RefusalError", execErr, execErr)
	}
	if got := len(fallbackRequests()); got != 0 {
		t.Errorf("requests to the fallback: got = %d, want = 0", got)
	}
}

// TestRefusalFallbackSuspensionNamesTheConfiguredModel: a call that fell back
// and then suspends seals the configured model's request, so a resume
// validates against the configured model and starts on it again.
func TestRefusalFallbackSuspensionNamesTheConfiguredModel(t *testing.T) {
	client, _ := newExecTestClient(t, scriptedTurns(t, "configured", "r"))
	fallbackClient, _ := newExecTestClient(t, func(int) []string {
		return wrapTurn("msg_ask", toolUseBlockSSE(t, 0, "toolu_ask", askAFriendToolName, map[string]any{"question": "ship it?"})...)
	})
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
		claudeexecutor.WithModel[errCapRequest, errCapResponse](configuredModel),
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](askAFriendProvider()),
		claudeexecutor.WithRefusalFallback[errCapRequest, errCapResponse](claudeexecutor.RefusalFallback{
			Messages:        fallbackClient.Messages,
			ProviderModelID: fallbackModel,
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{})

	susp, ok := checkpoint.AsSuspension(execErr)
	if !ok {
		t.Fatalf("Execute error: got %v, want a *checkpoint.Suspension", execErr)
	}
	if susp.Model != configuredModel {
		t.Errorf("Suspension.Model: got = %q, want = %q", susp.Model, configuredModel)
	}
	if got := requestModel(t, susp.ProviderState); got != configuredModel {
		t.Errorf("the sealed request's model: got = %q, want = %q", got, configuredModel)
	}
}

// TestRefusalFallbackRejectsAnIncompatibleModel pins New's check that the
// fallback takes the request built for the configured model.
func TestRefusalFallbackRejectsAnIncompatibleModel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		fallback   string
		opts       []claudeexecutor.Option[errCapRequest, errCapResponse]
		noMessages bool
		wantErr    string
	}{
		{name: "a zero Messages", configured: configuredModel, fallback: fallbackModel, noMessages: true, wantErr: "Messages is the zero value"},
		{name: "tool schemas with the reasoning parameter", configured: "claude-sonnet-5", fallback: "claude-haiku-5-5", wantErr: "reasoning parameter"},
		{
			name: "tools that keep the reasoning parameter by option", configured: configuredModel, fallback: fallbackModel,
			opts:    []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithToolReasoning[errCapRequest, errCapResponse](true)},
			wantErr: "reasoning parameter",
		},
		{name: "tool schemas without the reasoning parameter it does not need", configured: "claude-haiku-5-5", fallback: "claude-sonnet-5"},
		{name: "same request surface", configured: configuredModel, fallback: fallbackModel},
		{name: "the configured model itself", configured: configuredModel, fallback: configuredModel, wantErr: "is the configured model"},
		{name: "no sampling parameters", configured: "claude-sonnet-4-6", fallback: fallbackModel, wantErr: "sampling parameters"},
		{
			name: "a different effort", configured: "claude-opus-4-5", fallback: "claude-sonnet-4-5",
			opts:    []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithEffort[errCapRequest, errCapResponse](effort.High)},
			wantErr: "effort",
		},
		{
			name: "no forced tool choice", configured: "claude-sonnet-5", fallback: fallbackModel,
			opts:    []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithToolReasoning[errCapRequest, errCapResponse](false)},
			wantErr: "forced tool choice",
		},
		{name: "a forced tool choice it does not need", configured: configuredModel, fallback: "claude-sonnet-5"},
		{name: "not a Claude model", configured: configuredModel, fallback: "gemini-3.5-flash", wantErr: "Claude capability family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newExecTestClient(t, scriptedTurns(t, "configured", ""))
			prompt, err := promptbuilder.NewPrompt("hello")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			fallback := claudeexecutor.RefusalFallback{Messages: client.Messages, ProviderModelID: tc.fallback}
			if tc.noMessages {
				fallback.Messages = anthropic.MessageService{}
			}
			opts := append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithModel[errCapRequest, errCapResponse](tc.configured),
				claudeexecutor.WithRefusalFallback[errCapRequest, errCapResponse](fallback),
			}, tc.opts...)

			_, err = claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("New: got %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("New: got %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}

// labelServer answers every request with turns and records the
// X-Vertex-AI-Labels header each request carried, decoded.
func labelServer(t *testing.T, turns []string) (anthropic.MessageService, func() []map[string]string) {
	t.Helper()
	var (
		mu     sync.Mutex
		labels []map[string]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := map[string]string{}
		if raw, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Vertex-AI-Labels")); err != nil || json.Unmarshal(raw, &got) != nil {
			t.Errorf("X-Vertex-AI-Labels %q does not decode", r.Header.Get("X-Vertex-AI-Labels"))
		}
		mu.Lock()
		labels = append(labels, got)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, turns))
	}))
	t.Cleanup(srv.Close)
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	return client.Messages, func() []map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(labels)
	}
}

// TestRefusalFallbackServesUnderItsOwnRoute: a turn sent to the fallback
// carries the fallback's Vertex AI billing labels, is traced under the
// fallback's logical model (taken into the executor's attribution when the
// fallback names none), and is metered under the fallback's model. It sets
// the global meter provider, so it does not run in parallel.
func TestRefusalFallbackServesUnderItsOwnRoute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		if err := provider.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})

	configured, configuredLabels := labelServer(t, refusalTurn)
	fallback, fallbackLabels := labelServer(t, answerTurn)
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	attribution := agenttrace.Attribution{ProviderName: "gcp.vertex_ai", System: "gcp.vertex_ai", LogicalModel: configuredModel, Protocol: "anthropic-messages"}
	exec, err := claudeexecutor.NewWithMessages[errCapRequest, errCapResponse](configured, prompt,
		claudeexecutor.WithRoutedModel[errCapRequest, errCapResponse](configuredModel, configuredModel),
		claudeexecutor.WithAttribution[errCapRequest, errCapResponse](attribution),
		claudeexecutor.WithResourceLabels[errCapRequest, errCapResponse](map[string]string{"model_name": configuredModel}),
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
		claudeexecutor.WithRefusalFallback[errCapRequest, errCapResponse](claudeexecutor.RefusalFallback{
			Messages:        fallback,
			ProviderModelID: fallbackModel,
			ResourceLabels:  map[string]string{"model_name": fallbackModel},
		}),
	)
	if err != nil {
		t.Fatalf("NewWithMessages: %v", err)
	}
	tracer := &recordingTracer{}

	if _, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for name, tc := range map[string]struct {
		labels []map[string]string
		want   string
	}{
		"configured": {labels: configuredLabels(), want: configuredModel},
		"fallback":   {labels: fallbackLabels(), want: fallbackModel},
	} {
		if len(tc.labels) != 1 || tc.labels[0]["model_name"] != tc.want {
			t.Errorf("%s requests' model_name labels: got %v, want one request labelled %q", name, tc.labels, tc.want)
		}
	}

	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 2 {
		t.Fatalf("trace turns: got %d traces, want one with 2 turns", len(tracer.traces))
	}
	for i, want := range []string{configuredModel, fallbackModel} {
		turn := tracer.traces[0].Turns[i]
		if turn.LogicalModel != want || turn.Provider != "gcp.vertex_ai" || turn.Protocol != "anthropic-messages" {
			t.Errorf("trace turn %d: got logical model %q, provider %q, protocol %q, want %q on gcp.vertex_ai over anthropic-messages",
				i, turn.LogicalModel, turn.Provider, turn.Protocol, want)
		}
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "genai.token.prompt" {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("genai.token.prompt data = %T, want Sum[int64]", metric.Data)
			}
			for _, point := range sum.DataPoints {
				model, _ := point.Attributes.Value(attribute.Key("model"))
				got[model.AsString()] += point.Value
			}
		}
	}
	// refusalTurn reports 10 input tokens, answerTurn 20.
	if diff := cmp.Diff(map[string]int64{configuredModel: 10, fallbackModel: 20}, got); diff != "" {
		t.Errorf("genai.token.prompt by model (-want, +got):\n%s", diff)
	}
}

// TestRefusalFallbackErrorHandsTheRefusalBack: a fallback request that fails
// outright, here with a non-retryable API error, leaves the refused turn to
// the nudge on the configured model, or fails the run with the refusal when
// no nudge is left. A fallback that cannot serve costs no recovery the nudge
// would have made.
func TestRefusalFallbackErrorHandsTheRefusalBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nudges     int
		configured string // the configured model's script
		wantAnswer bool
	}{
		{name: "the nudge recovers the refused turn", nudges: 1, configured: "ra", wantAnswer: true},
		{name: "no nudge fails the run with the refusal", configured: "r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, configuredRequests := newExecTestClient(t, scriptedTurns(t, "configured", tc.configured))
			var (
				mu       sync.Mutex
				fallback int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				fallback++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"scripted fallback failure"}}`)
			}))
			t.Cleanup(srv.Close)
			fallbackClient := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
			prompt, err := promptbuilder.NewPrompt("hello")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			opts := []claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithModel[errCapRequest, errCapResponse](configuredModel),
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
				claudeexecutor.WithRefusalFallback[errCapRequest, errCapResponse](claudeexecutor.RefusalFallback{
					Messages:        fallbackClient.Messages,
					ProviderModelID: fallbackModel,
				}),
			}
			if tc.nudges > 0 {
				opts = append(opts, claudeexecutor.WithRefusalNudge[errCapRequest, errCapResponse](tc.nudges))
			}
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tracer := &recordingTracer{}

			resp, execErr := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, nil)

			if tc.wantAnswer {
				if execErr != nil || resp.Answer != "42" {
					t.Fatalf("Execute: resp = %+v, err = %v, want the answer after the nudge", resp, execErr)
				}
			} else if refErr, ok := errors.AsType[*claudeexecutor.RefusalError](execErr); !ok || refErr.Category != "cyber" {
				t.Fatalf("Execute error: got %v (%T), want the refusal as a *claudeexecutor.RefusalError", execErr, execErr)
			}
			mu.Lock()
			if fallback != 1 {
				t.Errorf("requests to the fallback: got = %d, want = 1", fallback)
			}
			mu.Unlock()
			configured := configuredRequests()
			if got, want := len(configured), len(tc.configured); got != want {
				t.Fatalf("requests to the configured model: got = %d, want = %d", got, want)
			}
			if tc.wantAnswer && !strings.Contains(string(configured[1]), "response withheld by Anthropic's safety classifier") {
				t.Error("the configured model's retry does not carry the placeholder and the nudge")
			}

			if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) < 2 {
				t.Fatalf("trace turns: got %d traces, want one with the refused turn and the fallback's", len(tracer.traces))
			}
			failed := tracer.traces[0].Turns[1]
			if failed.Model != fallbackModel || len(failed.Errors) == 0 || !strings.Contains(failed.Errors[0], "refusal fallback "+fallbackModel+" failed") {
				t.Errorf("the fallback's turn: got model %q, errors %q, want the fallback's failure recorded", failed.Model, failed.Errors)
			}
			if failed.Failed == tc.wantAnswer {
				t.Errorf("the fallback's turn failed: got %t, want %t", failed.Failed, !tc.wantAnswer)
			}
		})
	}
}
