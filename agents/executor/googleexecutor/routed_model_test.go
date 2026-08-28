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
