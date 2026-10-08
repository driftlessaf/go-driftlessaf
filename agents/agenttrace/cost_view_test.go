/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"os"
	"strings"
	"testing"
)

// TestCostViewPricesGeminiCachedTokensOnce pins the cost view to
// googleexecutor's token contract: Gemini input_tokens include the cached
// tokens also recorded as cache_read_tokens, so every input-price term must
// subtract cache_read_tokens for Gemini rows or cached tokens bill twice.
func TestCostViewPricesGeminiCachedTokensOnce(t *testing.T) {
	raw, err := os.ReadFile("cost-view/iac/sql/agent_trace_costs.sql")
	if err != nil {
		t.Fatalf("read cost view: %v", err)
	}
	sql := string(raw)

	const billableInput = "GREATEST(COALESCE(turn.input_tokens, 0)\n" +
		"        - IF(STARTS_WITH(m.pricing_model, 'gemini-'), COALESCE(turn.cache_read_tokens, 0), 0), 0) *"
	inputTerms := strings.Count(sql, "p_std.input_price)")
	if inputTerms == 0 {
		t.Fatal("cost view has no p_std.input_price terms; update this test to the view's shape")
	}
	if got := strings.Count(sql, billableInput); got != inputTerms {
		t.Errorf("input-price terms that exclude Gemini cache reads: got = %d, want = %d (one per input-price term)", got, inputTerms)
	}
}
