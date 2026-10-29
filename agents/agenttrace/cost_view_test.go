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

// TestCostViewPricesCachedTokensOnce pins the cost view to the token
// contracts of googleexecutor and responsesexecutor: Gemini and GPT
// input_tokens include the cached tokens also recorded as cache_read_tokens,
// and GPT input_tokens include the written tokens also recorded as
// cache_creation_tokens. Every input-price term must subtract them for those
// rows, or they bill twice.
func TestCostViewPricesCachedTokensOnce(t *testing.T) {
	sql := readCostView(t)

	const billableInput = "GREATEST(COALESCE(turn.input_tokens, 0)\n" +
		"        - IF(STARTS_WITH(m.pricing_model, 'gemini-') OR STARTS_WITH(m.pricing_model, 'gpt-'), COALESCE(turn.cache_read_tokens, 0), 0)\n" +
		"        - IF(STARTS_WITH(m.pricing_model, 'gpt-'), COALESCE(turn.cache_creation_tokens, 0), 0), 0) *"
	inputTerms := strings.Count(sql, "p_std.input_price)")
	if inputTerms == 0 {
		t.Fatal("cost view has no p_std.input_price terms; update this test to the view's shape")
	}
	if got := strings.Count(sql, billableInput); got != inputTerms {
		t.Errorf("input-price terms that exclude cached and written tokens: got = %d, want = %d (one per input-price term)", got, inputTerms)
	}
}

// TestCostViewAppliesGPTLongContextToEveryTerm checks that every tier
// predicate moves GPT calls above 272K input tokens to the Large Context
// rates, so input, output and cache costs switch tiers together.
func TestCostViewAppliesGPTLongContextToEveryTerm(t *testing.T) {
	sql := readCostView(t)

	const gptTier = "OR (STARTS_WITH(m.pricing_model, 'gpt-') AND turn.input_tokens > 272000)"
	tierTerms := strings.Count(sql, "p_large.")
	if tierTerms == 0 {
		t.Fatal("cost view has no p_large terms; update this test to the view's shape")
	}
	if got := strings.Count(sql, gptTier); got != tierTerms {
		t.Errorf("tier predicates with the GPT 272K rule: got = %d, want = %d (one per Large Context term)", got, tierTerms)
	}
}

func readCostView(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("cost-view/iac/sql/agent_trace_costs.sql")
	if err != nil {
		t.Fatalf("read cost view: %v", err)
	}
	return string(raw)
}
