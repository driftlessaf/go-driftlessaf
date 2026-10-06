/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
)

func TestReduceToolResults(t *testing.T) {
	t.Parallel()

	big := func(extra string) string {
		return `{"next_offset":40,"truncated":true,` + extra + `"commits":"` + strings.Repeat("é", 4000) + `"}`
	}
	toolUse := func(id, name string) anthropic.MessageParam {
		return anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant, Content: []anthropic.ContentBlockParamUnion{
			{OfToolUse: &anthropic.ToolUseBlockParam{ID: id, Name: name, Input: map[string]any{}}},
		}}
	}
	toolResult := func(id, text string, isError bool) anthropic.MessageParam {
		tr := anthropic.ToolResultBlockParam{
			ToolUseID:    id,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
			Content:      []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: text}}},
		}
		if isError {
			tr.IsError = anthropic.Bool(true)
		}
		return anthropic.MessageParam{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{{OfToolResult: &tr}}}
	}
	messages := []anthropic.MessageParam{
		toolUse("a", "list_commits"), toolResult("a", big(""), false),
		toolUse("b", "submit_result"), toolResult("b", big(""), false),
		toolUse("c", "list_commits"), toolResult("c", big(""), true),
		toolUse("d", "list_commits"), toolResult("d", big(`"error":"x",`), false),
		toolUse("e", "list_commits"), toolResult("e", big(""), false),
	}
	text := func(i int) string { return messages[i].Content[0].OfToolResult.Content[0].OfText.Text }
	before := make([]string, len(messages))
	for i := range messages {
		if messages[i].Role == anthropic.MessageParamRoleUser {
			before[i] = text(i)
		}
	}

	got := reduceToolResults(messages, func(name string) bool { return name == "submit_result" }, 1<<40, true)
	if got != 1 {
		t.Fatalf("reduced: got = %d, want = 1", got)
	}

	var r reducedResult
	if err := json.Unmarshal([]byte(text(1)), &r); err != nil {
		t.Fatalf("reduced text is not JSON: %v", err)
	}
	if !r.ExecutorReduced || r.ToolUseID != "a" || r.Tool != "list_commits" || r.OriginalBytes != len(before[1]) {
		t.Errorf("reduced result: got = %+v", r)
	}
	if string(r.Fields["next_offset"]) != "40" || string(r.Fields["truncated"]) != "true" {
		t.Errorf("fields: got = %v, want next_offset and truncated", r.Fields)
	}
	if _, ok := r.Fields["commits"]; ok {
		t.Error("fields: long string commits kept")
	}
	if !strings.HasPrefix(before[1], r.Head) || !strings.HasSuffix(before[1], r.Tail) {
		t.Error("head or tail is not a cut of the original")
	}
	tr := messages[1].Content[0].OfToolResult
	if tr.ToolUseID != "a" || !hasBreakpoint(tr.CacheControl) {
		t.Errorf("reduced block lost its ToolUseID or cache marker: %+v", tr)
	}
	// Submit, IsError, error-key and latest-turn results stay byte-identical.
	for _, i := range []int{3, 5, 7, 9} {
		if text(i) != before[i] {
			t.Errorf("message %d: got reduced, want unchanged", i)
		}
	}
}

// sizedJSON marshals fields with a "padding" string that brings the encoding
// to exactly n bytes, so a fixture has the size of a production result.
func sizedJSON(t *testing.T, fields map[string]any, n int) string {
	t.Helper()
	fields["padding"] = ""
	base, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if len(base) > n {
		t.Fatalf("fixture base is %d bytes, over the wanted %d", len(base), n)
	}
	fields["padding"] = strings.Repeat("x", n-len(base))
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if len(out) != n {
		t.Fatalf("fixture: got = %d bytes, want = %d", len(out), n)
	}
	return string(out)
}

// listCommitsResult renders a list_commits result as a bounded page: commits
// with per-commit file paging, a top-level next_offset paging pointer and the
// limit_bytes bound.
func listCommitsResult(t *testing.T, page int) string {
	t.Helper()
	commits := make([]any, 0, 20)
	for c := range 20 {
		commits = append(commits, map[string]any{
			"sha":               fmt.Sprintf("%040x", page*100+c),
			"message":           "update dependency",
			"files_total":       300,
			"files_omitted":     100,
			"files_next_offset": 200,
			"message_truncated": false,
		})
	}
	return sizedJSON(t, map[string]any{
		"commits":     commits,
		"next_offset": (page + 1) * 20,
		"truncated":   true,
		"limit_bytes": 256000,
	}, 256000)
}

type fixtureResult struct {
	name    string
	id      string
	input   map[string]any
	text    string
	isError bool
	cache   bool
}

// buildConversation renders results as alternating assistant tool_use and
// user tool_result messages after a cached first user prompt.
func buildConversation(results []fixtureResult) []anthropic.MessageParam {
	messages := []anthropic.MessageParam{{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{{OfText: &anthropic.TextBlockParam{
			Text:         "Generate the manifest.",
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}},
	}}
	for _, r := range results {
		messages = append(messages, anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant, Content: []anthropic.ContentBlockParamUnion{
			{OfToolUse: &anthropic.ToolUseBlockParam{ID: r.id, Name: r.name, Input: r.input}},
		}})
		tr := anthropic.ToolResultBlockParam{
			ToolUseID: r.id,
			Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: r.text}}},
		}
		if r.isError {
			tr.IsError = anthropic.Bool(true)
		}
		if r.cache {
			tr.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		messages = append(messages, anthropic.MessageParam{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{{OfToolResult: &tr}}})
	}
	return messages
}

// repeatedToolFixture is five list_commits pages of 256,000 bytes and a
// submit rejection that carries validator findings, followed by a small
// latest-turn result.
func repeatedToolFixture(t *testing.T) []fixtureResult {
	t.Helper()
	results := make([]fixtureResult, 0, 7)
	for p := range 5 {
		results = append(results, fixtureResult{
			name:  "list_commits",
			id:    fmt.Sprintf("toolu_lc%d", p+1),
			input: map[string]any{"offset": p * 20},
			text:  listCommitsResult(t, p),
			cache: p == 2,
		})
	}
	results = append(results, fixtureResult{
		name:  "submit_result",
		id:    "toolu_submit",
		input: map[string]any{"manifest": "draft"},
		text: sizedJSON(t, map[string]any{
			"status":   "rejected",
			"findings": []string{"pipeline step 3 references an undefined variable", "test block is missing"},
		}, 20000),
	}, fixtureResult{
		name:  "list_commits",
		id:    "toolu_latest",
		input: map[string]any{"offset": 100},
		text:  `{"commits":[],"truncated":false}`,
		cache: true,
	})
	return results
}

// largeInputFixture extends the repeated-tool fixture with a 256,000-byte
// read_build_logs tail, an IsError result, a result with an error key, a
// result whose reduction would not be smaller, and a 2,448,825-byte
// latest-turn result that is one unbounded page with no paging pointer.
func largeInputFixture(t *testing.T) []fixtureResult {
	t.Helper()
	results := repeatedToolFixture(t)
	results = results[:len(results)-1]
	return append(results,
		fixtureResult{
			name:  "read_build_logs",
			id:    "toolu_logs",
			input: map[string]any{"tail": true},
			text:  sizedJSON(t, map[string]any{"prev_offset": 1843200, "truncated": true}, 256000),
		},
		fixtureResult{
			name:    "get_file_diff",
			id:      "toolu_diff_err",
			input:   map[string]any{"path": "go.mod"},
			text:    sizedJSON(t, map[string]any{"detail": "diff unavailable"}, 10000),
			isError: true,
		},
		fixtureResult{
			name:  "get_file_diff",
			id:    "toolu_diff_key",
			input: map[string]any{"path": "go.sum"},
			text:  sizedJSON(t, map[string]any{"error": "path not found at ref"}, 10000),
		},
		fixtureResult{
			name:  "read_file",
			id:    "toolu_quotes",
			input: map[string]any{"path": "quotes.txt"},
			// Every quote doubles when escaped into head and tail, so the
			// reduced form is larger than the original.
			text: strings.Repeat(`"`, 4200),
		},
		fixtureResult{
			name:  "list_commits",
			id:    "toolu_latest",
			input: map[string]any{"offset": 0, "limit": 500},
			text:  sizedJSON(t, map[string]any{"commits": []any{}}, 2448825),
			cache: true,
		},
	)
}

func resultTexts(messages []anthropic.MessageParam) map[string]string {
	texts := make(map[string]string)
	for _, m := range messages {
		for _, b := range m.Content {
			if tr := b.OfToolResult; tr != nil {
				var sb strings.Builder
				for _, c := range tr.Content {
					if c.OfText != nil {
						sb.WriteString(c.OfText.Text)
					}
				}
				texts[tr.ToolUseID] = sb.String()
			}
		}
	}
	return texts
}

func cachePositions(messages []anthropic.MessageParam) []tailPosition {
	var positions []tailPosition
	for i := range messages {
		for j := range messages[i].Content {
			if cc := blockCacheControl(&messages[i].Content[j]); cc != nil && hasBreakpoint(*cc) {
				positions = append(positions, tailPosition{message: i, block: j})
			}
		}
	}
	return positions
}

// assertPaired fails unless every tool_use has exactly one tool_result in the
// next user message, which is what the API requires of a request.
func assertPaired(t *testing.T, messages []anthropic.MessageParam) {
	t.Helper()
	for i, m := range messages {
		for _, b := range m.Content {
			if b.OfToolUse == nil {
				continue
			}
			if i+1 >= len(messages) || messages[i+1].Role != anthropic.MessageParamRoleUser {
				t.Errorf("tool_use %q at message %d has no following user message", b.OfToolUse.ID, i)
				continue
			}
			n := 0
			for _, r := range messages[i+1].Content {
				if r.OfToolResult != nil && r.OfToolResult.ToolUseID == b.OfToolUse.ID {
					n++
				}
			}
			if n != 1 {
				t.Errorf("tool_use %q: got = %d tool_results, want = 1", b.OfToolUse.ID, n)
			}
		}
	}
}

func TestReduceToolResultsFixtures(t *testing.T) {
	t.Parallel()

	protected := func(name string) bool { return name == "submit_result" }
	tests := []struct {
		name    string
		fixture func(*testing.T) []fixtureResult
		// wantReduced maps each reduced tool_use id to the top-level paging
		// field its reduced form must keep.
		wantReduced map[string]string
	}{{
		name:    "repeated list_commits pages",
		fixture: repeatedToolFixture,
		wantReduced: map[string]string{
			"toolu_lc1": "next_offset", "toolu_lc2": "next_offset", "toolu_lc3": "next_offset",
			"toolu_lc4": "next_offset", "toolu_lc5": "next_offset",
		},
	}, {
		name:    "large input with an unbounded latest result",
		fixture: largeInputFixture,
		wantReduced: map[string]string{
			"toolu_lc1": "next_offset", "toolu_lc2": "next_offset", "toolu_lc3": "next_offset",
			"toolu_lc4": "next_offset", "toolu_lc5": "next_offset", "toolu_logs": "prev_offset",
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			results := tt.fixture(t)
			messages := buildConversation(results)
			before := resultTexts(messages)
			beforeCache := cachePositions(messages)
			beforeLen := len(messages)

			got := reduceToolResults(messages, protected, 1<<40, true)
			if got != len(tt.wantReduced) {
				t.Errorf("reduced: got = %d, want = %d", got, len(tt.wantReduced))
			}
			if len(messages) != beforeLen {
				t.Errorf("messages: got = %d, want = %d", len(messages), beforeLen)
			}
			assertPaired(t, messages)
			if diff := cmp.Diff(beforeCache, cachePositions(messages), cmp.AllowUnexported(tailPosition{})); diff != "" {
				t.Errorf("cache marker positions (-want, +got):\n%s", diff)
			}

			after := resultTexts(messages)
			for _, r := range results {
				pointer, reduced := tt.wantReduced[r.id]
				if !reduced {
					if after[r.id] != before[r.id] {
						t.Errorf("%s: got reduced, want byte-identical", r.id)
					}
					continue
				}
				var rr reducedResult
				if err := json.Unmarshal([]byte(after[r.id]), &rr); err != nil {
					t.Errorf("%s: reduced text is not JSON: %v", r.id, err)
					continue
				}
				var orig map[string]json.RawMessage
				if err := json.Unmarshal([]byte(before[r.id]), &orig); err != nil {
					t.Fatalf("%s: fixture is not JSON: %v", r.id, err)
				}
				if !rr.ExecutorReduced || rr.Tool != r.name || rr.ToolUseID != r.id || rr.OriginalBytes != len(before[r.id]) {
					t.Errorf("%s: reduced header: got = {%t %q %q %d}, want = {true %q %q %d}",
						r.id, rr.ExecutorReduced, rr.Tool, rr.ToolUseID, rr.OriginalBytes, r.name, r.id, len(before[r.id]))
				}
				for _, k := range []string{pointer, "truncated"} {
					if string(rr.Fields[k]) != string(orig[k]) {
						t.Errorf("%s: fields[%q]: got = %s, want = %s", r.id, k, rr.Fields[k], orig[k])
					}
				}
				// files_next_offset is nested per commit and is not kept; the
				// pointer to it is the kept tool_use_id and the note to call
				// the tool again with that input.
				if !strings.Contains(rr.Note, r.name) || !strings.Contains(rr.Note, r.id) {
					t.Errorf("%s: note does not point at the tool_use: %q", r.id, rr.Note)
				}
				if !strings.HasPrefix(before[r.id], rr.Head) || !strings.HasSuffix(before[r.id], rr.Tail) || rr.Head == "" || rr.Tail == "" {
					t.Errorf("%s: head or tail is not a cut of the original", r.id)
				}
			}

			// The tool_use the note points at keeps its original input.
			for i, m := range messages {
				for _, b := range m.Content {
					if b.OfToolUse == nil {
						continue
					}
					want := results[(i-1)/2].input
					if diff := cmp.Diff(want, b.OfToolUse.Input); diff != "" {
						t.Errorf("tool_use %q input (-want, +got):\n%s", b.OfToolUse.ID, diff)
					}
				}
			}

			// A reduced result is not reduced again.
			if again := reduceToolResults(messages, protected, 1<<40, true); again != 0 {
				t.Errorf("second reduction: got = %d, want = 0", again)
			}
		})
	}
}

func TestReduceToolResultsOldestFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		excessTokens int64
		wantIDs      []string
	}{{
		name:         "one token over reduces only the oldest page",
		excessTokens: 1,
		wantIDs:      []string{"toolu_lc1"},
	}, {
		name: "an excess above one page's estimated saving reduces the next oldest",
		// One page saves about 253,000 bytes, so 64,000 tokens at 4 bytes
		// per token, which is more than one page covers.
		excessTokens: 64000,
		wantIDs:      []string{"toolu_lc1", "toolu_lc2"},
	}, {
		name:         "an excess no saving covers reduces every eligible result",
		excessTokens: 1 << 40,
		wantIDs:      []string{"toolu_lc1", "toolu_lc2", "toolu_lc3", "toolu_lc4", "toolu_lc5"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			messages := buildConversation(repeatedToolFixture(t))
			before := resultTexts(messages)
			if got := reduceToolResults(messages, func(name string) bool { return name == "submit_result" }, tt.excessTokens, false); got != len(tt.wantIDs) {
				t.Errorf("reduced: got = %d, want = %d", got, len(tt.wantIDs))
			}
			after := resultTexts(messages)
			var gotIDs []string
			for _, id := range []string{"toolu_lc1", "toolu_lc2", "toolu_lc3", "toolu_lc4", "toolu_lc5", "toolu_submit", "toolu_latest"} {
				if after[id] != before[id] {
					gotIDs = append(gotIDs, id)
				}
			}
			if diff := cmp.Diff(tt.wantIDs, gotIDs); diff != "" {
				t.Errorf("reduced ids (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestReduceToolResultsKeepsWhole(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("a", 8000)
	tests := []struct {
		name     string
		messages func() []anthropic.MessageParam
	}{{
		name: "no assistant message",
		messages: func() []anthropic.MessageParam {
			return []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewToolResultBlock("toolu_x", big, false))}
		},
	}, {
		name: "result with no paired tool_use",
		messages: func() []anthropic.MessageParam {
			return []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewToolResultBlock("toolu_orphan", big, false)),
				anthropic.NewAssistantMessage(anthropic.NewTextBlock("thinking")),
				anthropic.NewUserMessage(anthropic.NewTextBlock("continue")),
			}
		},
	}, {
		name: "result smaller than the reducible minimum",
		messages: func() []anthropic.MessageParam {
			return buildConversation([]fixtureResult{
				{name: "list_commits", id: "toolu_a", input: map[string]any{}, text: strings.Repeat("a", minReducibleBytes-1)},
				{name: "list_commits", id: "toolu_b", input: map[string]any{}, text: "{}"},
			})
		},
	}, {
		name: "result that is not text only",
		messages: func() []anthropic.MessageParam {
			messages := buildConversation([]fixtureResult{
				{name: "list_commits", id: "toolu_a", input: map[string]any{}, text: big},
				{name: "list_commits", id: "toolu_b", input: map[string]any{}, text: "{}"},
			})
			tr := messages[2].Content[0].OfToolResult
			tr.Content = append(tr.Content, anthropic.ToolResultBlockParamContentUnion{
				OfImage: &anthropic.ImageBlockParam{Source: anthropic.ImageBlockParamSourceUnion{
					OfURL: &anthropic.URLImageSourceParam{URL: "https://example.com/a.png"},
				}},
			})
			return messages
		},
	}, {
		name: "suspend tool result",
		messages: func() []anthropic.MessageParam {
			return buildConversation([]fixtureResult{
				{name: "ask_human", id: "toolu_a", input: map[string]any{}, text: big},
				{name: "list_commits", id: "toolu_b", input: map[string]any{}, text: "{}"},
			})
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			messages := tt.messages()
			before := resultTexts(messages)
			if got := reduceToolResults(messages, func(name string) bool { return name == "ask_human" }, 1<<40, true); got != 0 {
				t.Errorf("reduced: got = %d, want = 0", got)
			}
			if diff := cmp.Diff(before, resultTexts(messages)); diff != "" {
				t.Errorf("result texts (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestReduceToolResultsKeepsNestedFindings(t *testing.T) {
	t.Parallel()

	// The result form of a log analysis tool: the findings sit in a nested
	// analysis object, and producing them again costs a model call.
	const errorMessage = "fatal error: openssl/ssl.h: No such file or directory"
	summary := strings.Repeat("The package build stopped during compilation. ", 70)
	body, err := json.Marshal(map[string]any{
		"kind":       "ci-check",
		"identifier": "build-check",
		"analysis": map[string]any{
			"summary": summary,
			"failures": []map[string]any{{
				"type":          "build",
				"error_message": errorMessage,
				"location":      map[string]any{"file": "src/module.c", "line": 3},
				"context": []map[string]any{{
					"content":      strings.Repeat("compiling src/module.c\n", 200),
					"why_relevant": "Compiler output from the failed build.",
				}},
			}},
			"confidence_score": 0.9,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := buildConversation([]fixtureResult{
		{name: "analyze_finding_logs", id: "toolu_a", input: map[string]any{}, text: string(body)},
		{name: "read_file", id: "toolu_b", input: map[string]any{}, text: "{}"},
	})
	if got := reduceToolResults(messages, func(string) bool { return false }, 1<<40, true); got != 1 {
		t.Fatalf("reduced: got = %d, want = 1", got)
	}
	text := resultTexts(messages)["toolu_a"]
	if len(text) >= len(body) {
		t.Errorf("reduced bytes: got = %d, want < %d", len(text), len(body))
	}
	var rr reducedResult
	if err := json.Unmarshal([]byte(text), &rr); err != nil {
		t.Fatalf("reduced text is not JSON: %v", err)
	}
	want := []reducedFinding{
		{Path: "analysis.failures[0].error_message", Text: errorMessage},
		{Path: "analysis.summary", Text: summary[:maxFindingBytes], Truncated: true},
	}
	if diff := cmp.Diff(want, rr.Findings); diff != "" {
		t.Errorf("findings (-want, +got):\n%s", diff)
	}
}

func TestReducedFindingsBound(t *testing.T) {
	t.Parallel()

	repeated := func(n int, v string) []any {
		out := make([]any, 0, n)
		for i := range n {
			out = append(out, fmt.Sprintf("%d%s", i, v))
		}
		return out
	}
	padding := strings.Repeat("p", 8000)
	tests := []struct {
		name   string
		result map[string]any
		// want maps paths the findings must keep to their text.
		want map[string]string
	}{{
		name:   "long errors cut at the total",
		result: map[string]any{"result": map[string]any{"errors": repeated(10, strings.Repeat("e", 400))}, "summary": "short", "padding": padding},
		want:   map[string]string{"result.errors[0]": "0" + strings.Repeat("e", 400)},
	}, {
		name:   "many one-byte errors",
		result: map[string]any{"errors": repeated(5000, ""), "padding": padding},
		want:   map[string]string{"errors[0]": "0"},
	}, {
		name:   "long key in every path",
		result: map[string]any{"result": map[string]any{strings.Repeat("k", 2000): map[string]any{"errors": repeated(20, "e")}}, "padding": padding},
	}, {
		name: "string nested beneath a finding key",
		result: map[string]any{"findings": []any{map[string]any{
			"message": "fatal error: openssl/ssl.h: No such file or directory",
		}}, "padding": padding},
		want: map[string]string{"findings[0].message": "fatal error: openssl/ssl.h: No such file or directory"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(tt.result)
			if err != nil {
				t.Fatal(err)
			}
			messages := buildConversation([]fixtureResult{
				{name: "analyze", id: "toolu_a", input: map[string]any{}, text: string(body)},
				{name: "read_file", id: "toolu_b", input: map[string]any{}, text: "{}"},
			})
			if got := reduceToolResults(messages, func(string) bool { return false }, 1<<40, true); got != 1 {
				t.Fatalf("reduced: got = %d, want = 1", got)
			}
			text := resultTexts(messages)["toolu_a"]
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(text), &raw); err != nil {
				t.Fatalf("reduced text is not JSON: %v", err)
			}
			if len(raw["findings"]) == 0 || len(raw["findings"]) > maxFindingsBytes {
				t.Errorf("findings JSON: got = %d bytes, want = 1 to %d", len(raw["findings"]), maxFindingsBytes)
			}
			var findings []reducedFinding
			if err := json.Unmarshal(raw["findings"], &findings); err != nil {
				t.Fatalf("findings: %v", err)
			}
			got := make(map[string]string, len(findings))
			for _, f := range findings {
				if f.Path == "summary" {
					t.Error("findings: top-level summary kept twice, want it in fields only")
				}
				if len(f.Path) > maxFindingPathBytes || !utf8.ValidString(f.Path) {
					t.Errorf("findings path: got = %d bytes, want valid UTF-8 of at most %d", len(f.Path), maxFindingPathBytes)
				}
				got[f.Path] = f.Text
			}
			for path, want := range tt.want {
				if got[path] != want {
					t.Errorf("findings[%q]: got = %q, want = %q", path, got[path], want)
				}
			}
		})
	}
}

func TestReducedResultTextRuneSafe(t *testing.T) {
	t.Parallel()

	// A three-byte rune puts the 1,024-byte cuts inside a rune unless the
	// cut moves to a rune boundary.
	text := "a" + strings.Repeat("€", 3000) + "b"
	got, ok := reducedResultText("read_file", "toolu_r", text, nil)
	if !ok {
		t.Fatal("reducedResultText: got not ok")
	}
	var rr reducedResult
	if err := json.Unmarshal([]byte(got), &rr); err != nil {
		t.Fatalf("reduced text is not JSON: %v", err)
	}
	if !utf8.ValidString(rr.Head) || !utf8.ValidString(rr.Tail) {
		t.Errorf("head or tail is not valid UTF-8: head %d bytes, tail %d bytes", len(rr.Head), len(rr.Tail))
	}
	if !strings.HasPrefix(text, rr.Head) || !strings.HasSuffix(text, rr.Tail) {
		t.Error("head or tail is not a cut of the original")
	}
	if len(rr.Head) > reducedEdgeBytes || len(rr.Tail) > reducedEdgeBytes || len(rr.Head) < reducedEdgeBytes-3 || len(rr.Tail) < reducedEdgeBytes-3 {
		t.Errorf("head and tail: got = %d and %d bytes, want = within 3 bytes under %d", len(rr.Head), len(rr.Tail), reducedEdgeBytes)
	}
}

func TestScalarFieldsBound(t *testing.T) {
	t.Parallel()

	obj := make(map[string]json.RawMessage, 22)
	for i := range 20 {
		obj[fmt.Sprintf("k%02d", i)] = json.RawMessage(fmt.Sprint(i))
	}
	obj["a_long"] = json.RawMessage(`"` + strings.Repeat("s", maxReducedFieldBytes+1) + `"`)
	obj["a_object"] = json.RawMessage(`{"next_offset":1}`)
	got := scalarFields(obj)
	want := make(map[string]json.RawMessage, maxReducedFields)
	for i := range maxReducedFields {
		want[fmt.Sprintf("k%02d", i)] = json.RawMessage(fmt.Sprint(i))
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("scalarFields (-want, +got):\n%s", diff)
	}
}

func TestInputBudgetArithmetic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		window    int64
		maxTokens int64
		want      int64
	}{
		{name: "opus 5.5 at the metaagent default", window: 1_000_000, maxTokens: 32000, want: 958000},
		{name: "opus 4.8 at the executor default", window: 1_000_000, maxTokens: 8192, want: 981808},
		{name: "a window at the output ceiling", window: 1_000_000, maxTokens: 128000, want: 862000},
		{name: "a 200,000 override", window: 200000, maxTokens: 32000, want: 166000},
		{name: "headroom that fills the window", window: 129000, maxTokens: 128000, want: -290},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := inputBudget(tt.window, tt.maxTokens); got != tt.want {
				t.Errorf("inputBudget(%d, %d): got = %d, want = %d", tt.window, tt.maxTokens, got, tt.want)
			}
		})
	}
}

func TestCountParams(t *testing.T) {
	t.Parallel()

	cached := anthropic.NewCacheControlEphemeralParam()
	params := anthropic.MessageNewParams{
		Model:       "claude-opus-5-5",
		MaxTokens:   32000,
		Temperature: anthropic.Float(0.1),
		System:      []anthropic.TextBlockParam{{Text: "system prompt", CacheControl: cached}},
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:         "list_commits",
			InputSchema:  anthropic.ToolInputSchemaParam{Properties: map[string]any{}},
			CacheControl: cached,
		}}},
		ToolChoice: anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}},
		Thinking:   anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		Messages:   []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))},
	}
	cp, err := countParams(params)
	if err != nil {
		t.Fatalf("countParams: %v", err)
	}
	body, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if diff := cmp.Diff([]string{"messages", "model", "system", "thinking", "tool_choice", "tools"}, slices.Sorted(maps.Keys(got))); diff != "" {
		t.Errorf("count request fields (-want, +got):\n%s", diff)
	}
	for _, k := range []string{"system", "tools"} {
		if !strings.Contains(string(got[k]), `"cache_control"`) {
			t.Errorf("%s: got = %s, want a cache_control marker", k, got[k])
		}
	}
	if bound := upperBoundTokens(cp); bound != int64(len(body))+countFramingTokens {
		t.Errorf("upperBoundTokens: got = %d, want = %d", bound, int64(len(body))+countFramingTokens)
	}

	// A tool the count request cannot carry fails the count instead of
	// undercounting the request.
	params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfBashTool20250124: &anthropic.ToolBash20250124Param{}})
	if _, err := countParams(params); err == nil {
		t.Error("countParams with a server tool: got nil error, want an error")
	}
}

func TestInputBudgetMediaSkipsLocalBound(t *testing.T) {
	t.Parallel()

	image := anthropic.NewImageBlockBase64("image/png", "iVBORw0KGgo=")
	document := anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: "JVBERi0xLjQ="})
	tests := []struct {
		name       string
		content    []anthropic.ContentBlockParamUnion
		wantCounts int
	}{{
		name:    "a small text-only request makes no count call",
		content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("hello")},
	}, {
		name:       "a small request with an image block is counted",
		content:    []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("hello"), image},
		wantCounts: 1,
	}, {
		name:       "a small request with a document block is counted",
		content:    []anthropic.ContentBlockParamUnion{document},
		wantCounts: 1,
	}, {
		name: "a small request with an image inside a tool result is counted",
		content: []anthropic.ContentBlockParamUnion{{OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: "toolu_1",
			Content:   []anthropic.ToolResultBlockParamContentUnion{{OfImage: image.OfImage}},
		}}},
		wantCounts: 1,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var counts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages/count_tokens" {
					t.Errorf("unexpected request path %q", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				counts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"input_tokens":10}`)
			}))
			t.Cleanup(srv.Close)
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
			e := &executor[*testBindable, *testResponse]{messages: client.Messages, modelName: "claude-opus-5-5", contextWindow: 1_000_000}
			params := anthropic.MessageNewParams{
				Model:     "claude-opus-5-5",
				MaxTokens: 32000,
				Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(tt.content...)},
			}
			if err := e.enforceInputBudget(t.Context(), retry.DefaultRetryConfig(), &params, func(string) bool { return false }); err != nil {
				t.Fatalf("enforceInputBudget: %v", err)
			}
			if got := int(counts.Load()); got != tt.wantCounts {
				t.Errorf("count calls: got = %d, want = %d", got, tt.wantCounts)
			}
		})
	}
}

func TestInputTooLargeErrorIsNotRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "bare", err: &InputTooLargeError{Model: "claude-opus-5-5", ContextWindow: 1_000_000, OutputHeadroom: 32000, Budget: 958000, InputTokens: 1_200_000, ReducedInputTokens: 990_000, ReducedResults: 3}},
		{name: "wrapped", err: fmt.Errorf("turn 4: %w", &InputTooLargeError{Model: "claude-opus-5-5"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if isRetryableClaudeError(tt.err) {
				t.Errorf("isRetryableClaudeError(%v): got = true, want = false", tt.err)
			}
		})
	}
}
