/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/metrics"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestAskRecordsMetrics swaps the global meter provider, so it must not run
// in parallel with other tests.
func TestAskRecordsMetrics(t *testing.T) {
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

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(sampleBody(sampleAnswers)))
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient("k",
		WithEndpoint(srv.URL),
		WithHTTPClient(srv.Client()),
		WithRetryConfig(fastRetry(1)),
		WithMetrics(metrics.NewGenAI(t.Name())),
		WithResourceLabels(map[string]string{"service_name": "systemone-test"}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Ask(t.Context(), sampleRequest()); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	// One increment per HTTP attempt, keyed by the observed response code.
	requests := make(map[string]int64)
	tokens := make(map[string]int64)
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				assertAttr(t, metric.Name, point.Attributes, "model", ModelJevLatest)
				assertAttr(t, metric.Name, point.Attributes, "gen_ai.provider.name", ProviderName)
				assertAttr(t, metric.Name, point.Attributes, "service_name", "systemone-test")
				switch metric.Name {
				case "genai.api.requests":
					code, _ := point.Attributes.Value(attribute.Key("response_code"))
					requests[code.AsString()] += point.Value
				case "genai.token.prompt", "genai.token.completion":
					tokens[metric.Name] += point.Value
				}
			}
		}
	}
	if requests["429"] != 1 || requests["200"] != 1 || len(requests) != 2 {
		t.Errorf("genai.api.requests by response_code: got = %v, want = {429:1 200:1}", requests)
	}
	if tokens["genai.token.prompt"] != 120 || tokens["genai.token.completion"] != 3 {
		t.Errorf("token counters: got = %v, want = {prompt:120 completion:3}", tokens)
	}
}

func assertAttr(t *testing.T, metricName string, set attribute.Set, key, want string) {
	t.Helper()
	got, ok := set.Value(attribute.Key(key))
	if !ok || got.AsString() != want {
		t.Errorf("%s attribute %q: got = %q (present=%v), want = %q", metricName, key, got.AsString(), ok, want)
	}
}
