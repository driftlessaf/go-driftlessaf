/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestCostViewPricesOneHourCacheWrites checks that every cache-write term
// applies the 1h TTL multiplier, and that every Claude price row keeps the 5m
// write at 1.25x input, so 1.6x the 5m rate is the 2x-input 1h rate.
func TestCostViewPricesOneHourCacheWrites(t *testing.T) {
	sql := readCostView(t)

	const oneHour = "IF(STARTS_WITH(m.pricing_model, 'claude-') AND turn.cache_ttl_seconds >= 3600, 1.6, 1) *"
	writeTerms := strings.Count(sql, "p_std.cache_creation_price)")
	if writeTerms == 0 {
		t.Fatal("cost view has no p_std.cache_creation_price terms; update this test to the view's shape")
	}
	if got := strings.Count(sql, oneHour); got != writeTerms {
		t.Errorf("cache-write terms with the 1h TTL multiplier: got = %d, want = %d (one per cache-write term)", got, writeTerms)
	}

	var claudeRows int
	for _, f := range structRows(sql) {
		if len(f) == 0 || !strings.HasPrefix(f[0], "'claude-") {
			continue
		}
		claudeRows++
		// pricing_model, pricing_tier, pricing_provider, input_price,
		// output_price, cache_read_price, cache_creation_price.
		if len(f) != 7 {
			t.Errorf("Claude price row %v: got %d fields, want 7", f, len(f))
			continue
		}
		input, err := strconv.ParseFloat(f[3], 64)
		if err != nil {
			t.Fatalf("%s %s input price %q: %v", f[0], f[1], f[3], err)
		}
		write, err := strconv.ParseFloat(f[6], 64)
		if err != nil {
			t.Fatalf("%s %s cache write price %q: %v", f[0], f[1], f[6], err)
		}
		if ratio := write / input; ratio < 1.2499 || ratio > 1.2501 {
			t.Errorf("%s %s %s: 5m cache write = %gx input, want = 1.25x", f[0], f[1], f[2], ratio)
		}
	}
	if claudeRows == 0 {
		t.Fatal("cost view has no Claude price rows; update this test to the view's shape")
	}
}

// structRows returns the top-level fields of every STRUCT(...) literal in
// sql, with comments and `AS name` aliases removed, so the long form
// that names the columns parses the same as the short rows after it.
func structRows(sql string) [][]string {
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		lines[i], _, _ = strings.Cut(line, "--")
	}
	sql = strings.Join(lines, "\n")

	var rows [][]string
	for _, body := range strings.Split(sql, "STRUCT(")[1:] {
		var fields []string
		depth, start := 0, 0
	scan:
		for i := range len(body) {
			switch body[i] {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
					continue
				}
				fields = append(fields, body[start:i])
				break scan
			case ',':
				if depth == 0 {
					fields = append(fields, body[start:i])
					start = i + 1
				}
			}
		}
		for i, f := range fields {
			f = strings.Join(strings.Fields(f), " ")
			// The alias follows the last " AS "; CAST(x AS T) has no alias after it.
			if j := strings.LastIndex(f, " AS "); j >= 0 && !strings.Contains(f[j:], ")") {
				f = f[:j]
			}
			fields[i] = f
		}
		rows = append(rows, fields)
	}
	return rows
}

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
