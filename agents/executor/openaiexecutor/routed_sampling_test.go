/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

import (
	"encoding/json"
	"testing"
)

func TestWithoutTemperatureOmitsRoutedRequestSampling(t *testing.T) {
	t.Parallel()

	exec := &executor[*testRequest, testResponse]{
		modelName:       "opaque-provider-deployment",
		maxTokens:       8192,
		temperature:     0.1,
		tokenLimitParam: TokenLimitMaxCompletionTokens,
	}
	if err := WithoutTemperature[*testRequest, testResponse]()(exec); err != nil {
		t.Fatalf("WithoutTemperature: %v", err)
	}
	params := exec.requestParams(nil, nil)
	if params.Temperature.Valid() {
		t.Errorf("Temperature = %v, want omitted", params.Temperature.Value)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("Marshal request params: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal request params: %v", err)
	}
	if _, ok := fields["temperature"]; ok {
		t.Errorf("request includes narrowed temperature: %s", encoded)
	}
}
