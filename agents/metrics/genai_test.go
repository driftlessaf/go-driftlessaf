/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestOperationName swaps the global meter provider, so neither it nor its
// subtests may run in parallel.
func TestOperationName(t *testing.T) {
	tests := []struct {
		name      string
		set       bool
		operation string
		want      string
	}{{
		name: "unset defaults to invoke_agent",
		want: "invoke_agent",
	}, {
		name: "empty keeps the default",
		set:  true,
		want: "invoke_agent",
	}, {
		name:      "context overrides the default",
		set:       true,
		operation: "batch_predict",
		want:      "batch_predict",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			previous := otel.GetMeterProvider()
			otel.SetMeterProvider(provider)
			t.Cleanup(func() { otel.SetMeterProvider(previous) })

			ctx := t.Context()
			if tt.set {
				ctx = WithOperationName(ctx, tt.operation)
			}
			m := NewGenAI(t.Name())
			// A caller attribute must not be able to set the operation.
			m.RecordTokens(ctx, "m", 3, 4, attribute.String("gen_ai.operation.name", "spoofed"))
			m.RecordCacheTokens(ctx, "m", 5, 6)

			var data metricdata.ResourceMetrics
			if err := reader.Collect(t.Context(), &data); err != nil {
				t.Fatal(err)
			}
			points := 0
			for _, scope := range data.ScopeMetrics {
				for _, metric := range scope.Metrics {
					hist, ok := metric.Data.(metricdata.Histogram[float64])
					if !ok || metric.Name != "gen_ai.client.token.usage" {
						continue
					}
					for _, point := range hist.DataPoints {
						points++
						got, _ := point.Attributes.Value("gen_ai.operation.name")
						if got.AsString() != tt.want {
							t.Errorf("gen_ai.operation.name: got = %q, want = %q", got.AsString(), tt.want)
						}
					}
				}
			}
			// input, output, cache_read, cache_creation.
			if points != 4 {
				t.Errorf("semconv data points: got = %d, want = 4", points)
			}
		})
	}
}
