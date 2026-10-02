/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"encoding/json"
	"testing"
)

func TestWithRoutedModelSeparatesWireAndCapabilityIdentities(t *testing.T) {
	t.Parallel()

	exec := &executor[*testBindable, *testResponse]{}
	if err := WithRoutedModel[*testBindable, *testResponse](
		"opaque-provider-deployment",
		"gemini-3-flash-preview",
	)(exec); err != nil {
		t.Fatalf("WithRoutedModel: %v", err)
	}
	if got, want := exec.model, "opaque-provider-deployment"; got != want {
		t.Errorf("wire model = %q, want %q", got, want)
	}
	if got, want := exec.capabilityModel, "gemini-3-flash-preview"; got != want {
		t.Errorf("capability model = %q, want %q", got, want)
	}
	if !usesThinkingLevel(exec.capabilityModel) {
		t.Error("logical Gemini 3 model did not select thinking-level capabilities")
	}
}

func TestWithoutTemperatureOmitsRoutedRequestSampling(t *testing.T) {
	t.Parallel()

	exec := &executor[*testBindable, *testResponse]{
		temperature:     0.1,
		maxOutputTokens: 8192,
	}
	for _, option := range []Option[*testBindable, *testResponse]{
		WithRoutedModel[*testBindable, *testResponse]("opaque-provider-deployment", "gemini-2.5-flash"),
		WithoutTemperature[*testBindable, *testResponse](),
	} {
		if err := option(exec); err != nil {
			t.Fatalf("applying routed option: %v", err)
		}
	}
	config := exec.generationConfig()
	if config.Temperature != nil {
		t.Errorf("Temperature = %v, want omitted", *config.Temperature)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal generation config: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal generation config: %v", err)
	}
	if _, ok := fields["temperature"]; ok {
		t.Errorf("request includes narrowed temperature: %s", encoded)
	}
}

func TestDefaultTemperatureFollowsGeminiGeneration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		model    string
		options  []Option[*testBindable, *testResponse]
		wantTemp *float32
	}{{
		name:     "Gemini 2.x default",
		model:    "gemini-2.5-flash",
		wantTemp: new(float32(0.1)),
	}, {
		name:  "Gemini 3 default omitted",
		model: "gemini-3.8-flash",
	}, {
		name:     "Gemini 3 explicit temperature sent",
		model:    "gemini-3.8-flash",
		options:  []Option[*testBindable, *testResponse]{WithTemperature[*testBindable, *testResponse](0.7)},
		wantTemp: new(float32(0.7)),
	}, {
		name:    "Gemini 3 explicit then omitted",
		model:   "gemini-3.8-flash",
		options: []Option[*testBindable, *testResponse]{WithTemperature[*testBindable, *testResponse](0.7), WithoutTemperature[*testBindable, *testResponse]()},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := &executor[*testBindable, *testResponse]{temperature: 0.1, maxOutputTokens: 8192}
			for _, option := range append([]Option[*testBindable, *testResponse]{WithModel[*testBindable, *testResponse](tc.model)}, tc.options...) {
				if err := option(exec); err != nil {
					t.Fatalf("applying option: %v", err)
				}
			}
			got := exec.generationConfig().Temperature
			switch {
			case tc.wantTemp == nil && got != nil:
				t.Errorf("Temperature: got = %v, want = omitted", *got)
			case tc.wantTemp != nil && got == nil:
				t.Errorf("Temperature: got = omitted, want = %v", *tc.wantTemp)
			case tc.wantTemp != nil && *got != *tc.wantTemp:
				t.Errorf("Temperature: got = %v, want = %v", *got, *tc.wantTemp)
			}
		})
	}
}
