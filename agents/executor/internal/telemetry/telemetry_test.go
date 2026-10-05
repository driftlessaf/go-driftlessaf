/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package telemetry

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestResponseCodeAttr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		code int
		want string
	}{
		{name: "0 is success", code: 0, want: "200"},
		{name: "negative is unknown", code: -1, want: "unknown"},
		{name: "200 in-stream error with unrecognised body type", code: 200, want: "200"},
		{name: "429", code: 429, want: "429"},
		{name: "500", code: 500, want: "500"},
		{name: "503", code: 503, want: "503"},
		{name: "529", code: 529, want: "529"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := responseCodeAttr(tt.code); got != tt.want {
				t.Errorf("responseCodeAttr(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// TestWithAPIRequestCounter_PreservesBaseCallback locks in the composition
// behaviour of WithAPIRequestCounter: it must wrap the existing
// OnAttemptError, not overwrite it. Per-attempt trace recording relies on
// the original callback continuing to fire, so a future edit that drops the
// chaining would silently break llmTurn error capture.
func TestWithAPIRequestCounter_PreservesBaseCallback(t *testing.T) {
	t.Parallel()

	r := NewRecorder(metrics.NewGenAI("test"), "gemini-test", "", "gcp.vertex_ai", nil, func(error) int { return -1 })
	var got []error
	cfg := retry.RetryConfig{
		OnAttemptError: func(err error) { got = append(got, err) },
	}
	wrapped := r.WithAPIRequestCounter(t.Context(), cfg)

	sentinel := errors.New("transient")
	wrapped.OnAttemptError(sentinel)

	if len(got) != 1 || !errors.Is(got[0], sentinel) {
		t.Errorf("base OnAttemptError not invoked: got %v, want one call with sentinel", got)
	}
}

// TestWithAPIRequestCounter_NilBase ensures the wrapper handles configs
// that don't set OnAttemptError (the outer retry call site uses one). It
// must not panic and must still record the API request.
func TestWithAPIRequestCounter_NilBase(t *testing.T) {
	t.Parallel()

	r := NewRecorder(metrics.NewGenAI("test"), "gemini-test", "", "gcp.vertex_ai", nil, func(error) int { return -1 })
	cfg := retry.RetryConfig{} // OnAttemptError is nil
	wrapped := r.WithAPIRequestCounter(t.Context(), cfg)

	// Must not panic.
	wrapped.OnAttemptError(errors.New("boom"))
}

// TestRecordTokens_ModelLabel swaps the global meter provider, so neither it
// nor its subtests may run in parallel.
func TestRecordTokens_ModelLabel(t *testing.T) {
	const providerModel = "us.anthropic.claude-sonnet-5"
	tests := []struct {
		name          string
		reportedModel string
		wantModel     string
	}{{
		name:          "reported model overrides the model label",
		reportedModel: "claude-sonnet-5",
		wantModel:     "claude-sonnet-5",
	}, {
		name:          "empty reported model keeps the provider model",
		reportedModel: "",
		wantModel:     providerModel,
	}, {
		name:          "equal reported model keeps the provider model",
		reportedModel: providerModel,
		wantModel:     providerModel,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			previous := otel.GetMeterProvider()
			otel.SetMeterProvider(provider)
			t.Cleanup(func() {
				otel.SetMeterProvider(previous)
				if err := provider.Shutdown(t.Context()); err != nil {
					t.Error(err)
				}
			})

			r := NewRecorder(metrics.NewGenAI(t.Name()), providerModel, tt.reportedModel, "aws.bedrock", nil, nil)
			r.RecordTokens(t.Context(), 10, 5)

			var data metricdata.ResourceMetrics
			if err := reader.Collect(t.Context(), &data); err != nil {
				t.Fatal(err)
			}
			var got []map[string]string
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
						got = append(got, map[string]string{
							"model":                stringAttr(point.Attributes, "model"),
							"gen_ai.request.model": stringAttr(point.Attributes, "gen_ai.request.model"),
						})
					}
				}
			}
			want := []map[string]string{{
				"model":                tt.wantModel,
				"gen_ai.request.model": providerModel,
			}}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("genai.token.prompt attributes (-want +got):\n%s", diff)
			}
		})
	}
}

func stringAttr(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}
