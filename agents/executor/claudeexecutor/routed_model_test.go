/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import "testing"

func TestWithRoutedModelSeparatesWireAndCapabilityIdentities(t *testing.T) {
	t.Parallel()

	exec := &executor[*testBindable, *testResponse]{}
	if err := WithRoutedModel[*testBindable, *testResponse](
		"opaque-provider-deployment",
		"claude-sonnet-5",
	)(exec); err != nil {
		t.Fatalf("WithRoutedModel: %v", err)
	}
	if got, want := exec.modelName, "opaque-provider-deployment"; got != want {
		t.Errorf("wire model = %q, want %q", got, want)
	}
	if got, want := exec.capabilityModelName, "claude-sonnet-5"; got != want {
		t.Errorf("capability model = %q, want %q", got, want)
	}
	if supportsSamplingParams(exec.capabilityModelName) {
		t.Error("logical Sonnet 5 model unexpectedly selected legacy sampling capabilities")
	}
}

func TestWithoutTemperatureOmitsRoutedRequestSampling(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithRoutedModel[*testBindable, *testResponse]("opaque-provider-deployment", "claude-sonnet-4-6"),
		WithoutTemperature[*testBindable, *testResponse](),
	)
	params, _, err := exec.assembleParams("payload", "", nil)
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if params.Temperature.Valid() {
		t.Errorf("Temperature = %v, want omitted", params.Temperature.Value)
	}
}

func TestWithoutTemperatureKeepsProtocolRequiredThinkingTemperature(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t,
		WithRoutedModel[*testBindable, *testResponse]("opaque-provider-deployment", "claude-sonnet-4-6"),
		WithoutTemperature[*testBindable, *testResponse](),
		WithThinking[*testBindable, *testResponse](2048),
	)
	params, _, err := exec.assembleParams("payload", "", nil)
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if !params.Temperature.Valid() || params.Temperature.Value != 1.0 {
		t.Errorf("Temperature = %+v, want protocol-required 1.0", params.Temperature)
	}
}
