/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/go-cmp/cmp"
)

// budgetTurn is one assistant tool call and the user message answering it.
func budgetTurn(id, tool string, input map[string]any, result string) []anthropic.MessageParam {
	return []anthropic.MessageParam{{
		Role: anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{
			{OfToolUse: &anthropic.ToolUseBlockParam{ID: id, Name: tool, Input: input}},
		},
	}, {
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{{OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: id,
			Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: result}}},
		}}},
	}}
}

// planIDs returns the tool_use ids of rs in order.
func planIDs(rs []plannedReduction) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.result.ToolUseID)
	}
	return ids
}

// bigResult is a reducible result of about n bytes.
func bigResult(n int) string {
	return `{"content":"` + strings.Repeat("x", n) + `"}`
}

// budgetConversation is five answered turns, a to e, then the assistant turn
// f whose result the model has not read. The c result is too small to
// shorten, and d is the largest.
func budgetConversation() []anthropic.MessageParam {
	big := bigResult(3 * minReducibleBytes)
	messages := []anthropic.MessageParam{userTextMessage("task")}
	messages = append(messages, budgetTurn("a", "read_file", map[string]any{"path": "facts/manifest.json"}, big)...)
	messages = append(messages, budgetTurn("b", "read_file", map[string]any{"path": "pkg/index.js"}, big)...)
	messages = append(messages, budgetTurn("c", "read_file", map[string]any{"path": "pkg/small.js"}, `{"content":"short"}`)...)
	messages = append(messages, budgetTurn("d", "search", map[string]any{"pattern": "eval"}, bigResult(40*minReducibleBytes))...)
	messages = append(messages, budgetTurn("e", "read_file", map[string]any{"path": "pkg/late.js"}, big)...)
	messages = append(messages, budgetTurn("f", "read_file", map[string]any{"path": "pkg/unread.js"}, big)...)
	return messages
}

func protectNothing(string, json.RawMessage) bool { return false }

func TestPlanCompaction(t *testing.T) {
	t.Parallel()

	messages := budgetConversation()
	manifest := func(tool string, input json.RawMessage) bool {
		return tool == "read_file" && strings.Contains(string(input), "manifest.json")
	}

	for _, tc := range []struct {
		name       string
		keep       int
		protect    func(string, json.RawMessage) bool
		wantOlder  []string
		wantRecent []string
	}{{
		name:      "no keep window makes every read result older",
		protect:   protectNothing,
		wantOlder: []string{"a", "b", "d", "e"},
	}, {
		name:       "keep window holds the most recent answered turns",
		keep:       2,
		protect:    protectNothing,
		wantOlder:  []string{"a", "b"},
		wantRecent: []string{"d", "e"},
	}, {
		name:      "protected input stays whole",
		protect:   manifest,
		wantOlder: []string{"b", "d", "e"},
	}, {
		name:       "keep window and protection together",
		keep:       1,
		protect:    manifest,
		wantOlder:  []string{"b", "d"},
		wantRecent: []string{"e"},
	}, {
		name:       "keep window wider than the run",
		keep:       6,
		protect:    protectNothing,
		wantRecent: []string{"a", "b", "d", "e"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before, err := json.Marshal(messages)
			if err != nil {
				t.Fatal(err)
			}
			plan := planCompaction(messages, tc.keep, tc.protect)
			for _, r := range slices.Concat(plan.older, plan.recent, plan.unread) {
				if r.saved <= 0 {
					t.Errorf("reduction of %s: saved = %d, want a positive saving", r.result.ToolUseID, r.saved)
				}
				if messages[r.at.message].Content[r.at.block].OfToolResult.ToolUseID != r.result.ToolUseID {
					t.Errorf("reduction of %s points at another block", r.result.ToolUseID)
				}
			}
			if diff := cmp.Diff(tc.wantOlder, planIDs(plan.older)); diff != "" {
				t.Errorf("older (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantRecent, planIDs(plan.recent)); diff != "" {
				t.Errorf("recent (-want, +got):\n%s", diff)
			}
			// The newest turn's result is never in reach of a compaction.
			if diff := cmp.Diff([]string{"f"}, planIDs(plan.unread)); diff != "" {
				t.Errorf("unread (-want, +got):\n%s", diff)
			}
			after, err := json.Marshal(messages)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Error("planCompaction changed the messages")
			}
		})
	}
}

// TestCompactionPlanTake pins which results a compaction shortens for a given
// excess: every older one whatever the excess, then recent ones largest
// first and only while the excess is uncovered, and never an unread one.
func TestCompactionPlanTake(t *testing.T) {
	t.Parallel()

	// The keep window of 3 holds c (not reducible), d (the largest) and e.
	plan := planCompaction(budgetConversation(), 3, protectNothing)
	credit := func(rs ...plannedReduction) int64 {
		var saved int64
		for _, r := range rs {
			saved += int64(r.saved)
		}
		return saved / contextCreditBytesPerToken
	}
	older := credit(plan.older...)
	d, e := plan.recent[0], plan.recent[1]

	for _, tc := range []struct {
		name       string
		excess     int64
		want       []string
		wantRecent int
		wantRest   string
	}{{
		name:     "older results cover the excess and the window stays whole",
		excess:   older,
		want:     []string{"a", "b"},
		wantRest: "d",
	}, {
		name:     "every older result is taken when one would cover the excess",
		excess:   1,
		want:     []string{"a", "b"},
		wantRest: "d",
	}, {
		name:       "the largest recent result goes first and the rest stay",
		excess:     older + 1,
		want:       []string{"a", "b", "d"},
		wantRecent: 1,
		wantRest:   "e",
	}, {
		name:       "the largest recent result alone covers what it can",
		excess:     older + credit(d),
		want:       []string{"a", "b", "d"},
		wantRecent: 1,
		wantRest:   "e",
	}, {
		name:       "an excess nothing covers takes every read result",
		excess:     older + credit(d, e) + 1_000_000,
		want:       []string{"a", "b", "d", "e"},
		wantRecent: 2,
		wantRest:   "f",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			taken, recent, rest := plan.take(tc.excess)
			if diff := cmp.Diff(tc.want, planIDs(taken)); diff != "" {
				t.Errorf("taken (-want, +got):\n%s", diff)
			}
			if recent != tc.wantRecent {
				t.Errorf("recent: got = %d, want = %d", recent, tc.wantRecent)
			}
			if rest == nil {
				t.Fatalf("rest: got = nil, want the position of %s", tc.wantRest)
			}
			messages := budgetConversation()
			if got := messages[rest.message].Content[rest.block].OfToolResult.ToolUseID; got != tc.wantRest {
				t.Errorf("rest: got = %s, want = %s", got, tc.wantRest)
			}
		})
	}

	t.Run("nothing left to shorten has no rest", func(t *testing.T) {
		t.Parallel()
		messages := budgetConversation()
		// Drop the unread turn's result down to one too small to shorten.
		messages[len(messages)-1] = budgetTurn("f", "read_file", map[string]any{}, `{"content":"short"}`)[1]
		_, _, rest := planCompaction(messages, 0, protectNothing).take(1)
		if rest != nil {
			t.Errorf("rest: got = %+v, want = nil", *rest)
		}
	})
}

func TestContextAnchorEstimate(t *testing.T) {
	t.Parallel()

	turn := budgetTurn("a", "read_file", map[string]any{}, strings.Repeat("y", 1000))
	answer, appended := turn[0], turn[1]
	body, err := json.Marshal([]anthropic.MessageParam{appended})
	if err != nil {
		t.Fatal(err)
	}
	appendedTokens := int64(len(body)+1) / 2
	task := userTextMessage("task")

	for _, tc := range []struct {
		name     string
		anchor   contextAnchor
		messages []anthropic.MessageParam
		want     int64
	}{{
		name:     "nothing appended is the anchor",
		anchor:   contextAnchor{tokens: 5000, messages: 1},
		messages: []anthropic.MessageParam{task},
		want:     5000,
	}, {
		name:     "the answering assistant message is covered by the anchor",
		anchor:   contextAnchor{tokens: 5000, messages: 1},
		messages: []anthropic.MessageParam{task, answer},
		want:     5000,
	}, {
		name:     "appended results count at two bytes per token",
		anchor:   contextAnchor{tokens: 5000, messages: 1},
		messages: []anthropic.MessageParam{task, answer, appended},
		want:     5000 + appendedTokens,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.anchor.estimate(tc.messages); got != tc.want {
				t.Errorf("estimate: got = %d, want = %d", got, tc.want)
			}
		})
	}
}

// TestCompactToBudget pins when a request is compacted and how far: not
// without a provider count, not under the budget, and otherwise as far as
// the eligible results reach even when that leaves it over the budget.
func TestCompactToBudget(t *testing.T) {
	t.Parallel()

	// asked is budgetConversation with an answered ask_human turn h before
	// turn a.
	asked := func() []anthropic.MessageParam {
		return slices.Insert(budgetConversation(), 1,
			budgetTurn("h", "ask_human", map[string]any{"question": "proceed?"}, bigResult(3*minReducibleBytes))...)
	}
	// anchored is a provider count taken before the unread turn f, large
	// enough that no shortening brings the request under a 1,000-token
	// budget.
	anchored := contextAnchor{tokens: 500_000, messages: len(budgetConversation()) - 2}

	for _, tc := range []struct {
		name   string
		budget ContextBudget
		anchor contextAnchor
		// messages is the conversation, budgetConversation when nil.
		messages func() []anthropic.MessageParam
		// protectedTool is the tool the executor's own predicate protects.
		protectedTool string
		wantReduced   []string
		wantRecent    int
	}{{
		name:   "no provider count yet",
		budget: ContextBudget{Tokens: 1000},
	}, {
		name:   "estimate under the budget",
		budget: ContextBudget{Tokens: 1_000_000},
		anchor: anchored,
	}, {
		name:        "a budget out of reach still shortens every read result",
		budget:      ContextBudget{Tokens: 1000, KeepRecentTurns: 3},
		anchor:      anchored,
		wantReduced: []string{"a", "b", "d", "e"},
		wantRecent:  2,
	}, {
		name: "protected results stay whole when the budget is out of reach",
		budget: ContextBudget{Tokens: 1000, Protect: func(_ string, input json.RawMessage) bool {
			return strings.Contains(string(input), "manifest.json")
		}},
		anchor:      anchored,
		wantReduced: []string{"b", "d", "e"},
	}, {
		name:          "a human answer stays whole with Protect unset",
		budget:        ContextBudget{Tokens: 1000},
		anchor:        contextAnchor{tokens: 500_000, messages: len(asked()) - 2},
		messages:      asked,
		protectedTool: "ask_human",
		wantReduced:   []string{"a", "b", "d", "e"},
	}, {
		name:        "the low-water mark takes the window's largest result",
		budget:      ContextBudget{Tokens: 502_000, LowWaterTokens: 480_000, KeepRecentTurns: 3},
		anchor:      anchored,
		wantReduced: []string{"a", "b", "d"},
		wantRecent:  1,
	}, {
		name:        "without a low-water mark the window stays whole",
		budget:      ContextBudget{Tokens: 502_000, KeepRecentTurns: 3},
		anchor:      anchored,
		wantReduced: []string{"a", "b"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var compactions []ContextCompaction
			tc.budget.OnCompact = func(c ContextCompaction) { compactions = append(compactions, c) }
			e := newTestExecutor(t, WithContextBudget[*testBindable, *testResponse](tc.budget))
			messages := budgetConversation
			if tc.messages != nil {
				messages = tc.messages
			}
			params := anthropic.MessageNewParams{Messages: messages()}
			before := resultTexts(params.Messages)
			tail := &tailBreakpoints{limit: tailBreakpointLimit}

			got := e.compactToBudget(t.Context(), 7, &params, tc.anchor, func(name string) bool {
				return name != "" && name == tc.protectedTool
			}, tail)

			var reduced []string
			for _, m := range params.Messages {
				for _, b := range m.Content {
					tr := b.OfToolResult
					if tr == nil {
						continue
					}
					text := tr.Content[0].OfText.Text
					switch {
					case strings.Contains(text, `"executor_reduced":true`):
						reduced = append(reduced, tr.ToolUseID)
					case text != before[tr.ToolUseID]:
						t.Errorf("result %q: changed without being shortened", tr.ToolUseID)
					}
				}
			}
			if diff := cmp.Diff(tc.wantReduced, reduced); diff != "" {
				t.Errorf("shortened results (-want, +got):\n%s", diff)
			}
			if len(tc.wantReduced) == 0 {
				if len(compactions) != 0 || got != tc.anchor {
					t.Errorf("got %d compactions and anchor %+v, want none and the anchor unchanged", len(compactions), got)
				}
				return
			}
			if len(compactions) != 1 {
				t.Fatalf("compactions: got = %d, want = 1", len(compactions))
			}
			c := compactions[0]
			if c.Turn != 7 || c.ReducedResults != len(tc.wantReduced) || c.RecentResults != tc.wantRecent || c.SavedBytes <= 0 {
				t.Errorf("compaction: got = %+v, want turn 7, %d results, %d recent and a saving", c, len(tc.wantReduced), tc.wantRecent)
			}
			if want := tc.anchor.tokens - c.SavedBytes/contextCreditBytesPerToken; got.tokens != want || got.messages != tc.anchor.messages {
				t.Errorf("anchor: got = %+v, want %d tokens at the same message", got, want)
			}
		})
	}
}

// markedPositions returns the blocks of messages that carry a cache_control
// marker, in conversation order.
func markedPositions(messages []anthropic.MessageParam) []tailPosition {
	var marked []tailPosition
	for m := range messages {
		for b := range messages[m].Content {
			if cc := blockCacheControl(&messages[m].Content[b]); cc != nil && hasBreakpoint(*cc) {
				marked = append(marked, tailPosition{message: m, block: b})
			}
		}
	}
	return marked
}

// TestTailRepin pins where a compaction puts the two tail markers and the
// pin it leaves for the next one. In budgetConversation the task is message
// 0, turn a is messages 1 and 2, and each later turn is the next two.
func TestTailRepin(t *testing.T) {
	t.Parallel()

	at := func(message int) tailPosition { return tailPosition{message: message} }
	pinned := func(message int) *tailPosition { return new(at(message)) }

	for _, tc := range []struct {
		name      string
		limit     int
		pin       *tailPosition
		compacted bool
		first     tailPosition
		rest      *tailPosition
		want      []tailPosition
		wantPin   *tailPosition
	}{{
		name:    "first compaction reads an earlier tail and pins before the rest",
		limit:   tailBreakpointLimit,
		first:   at(2),
		rest:    pinned(8),
		want:    []tailPosition{at(0), at(7)},
		wantPin: pinned(7),
	}, {
		name:      "a later compaction reads the pin and moves it",
		limit:     tailBreakpointLimit,
		pin:       pinned(7),
		compacted: true,
		first:     at(8),
		rest:      pinned(12),
		want:      []tailPosition{at(7), at(11)},
		wantPin:   pinned(11),
	}, {
		name:      "nothing left to shorten pins the tail",
		limit:     tailBreakpointLimit,
		pin:       pinned(7),
		compacted: true,
		first:     at(8),
		want:      []tailPosition{at(7), at(12)},
		wantPin:   pinned(12),
	}, {
		name:      "a result left whole right after the pin keeps the pin",
		limit:     tailBreakpointLimit,
		pin:       pinned(7),
		compacted: true,
		first:     at(10),
		rest:      pinned(8),
		want:      []tailPosition{at(7), at(12)},
		wantPin:   pinned(7),
	}, {
		name:      "no pin after an earlier compaction leaves the tail markers",
		limit:     tailBreakpointLimit,
		compacted: true,
		first:     at(8),
		rest:      pinned(12),
		want:      []tailPosition{at(10), at(12)},
	}, {
		name:  "one tail marker is left where it is",
		limit: 1,
		first: at(2),
		rest:  pinned(8),
		want:  []tailPosition{at(12)},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			messages := budgetConversation()
			tb := &tailBreakpoints{limit: tc.limit, pin: tc.pin, compacted: tc.compacted}
			// The markers of the previous request and of this one.
			tb.advance(messages[:len(messages)-2])
			tb.advance(messages)

			tb.repin(messages, tc.first, tc.rest)

			if diff := cmp.Diff(tc.want, markedPositions(messages), cmp.AllowUnexported(tailPosition{})); diff != "" {
				t.Errorf("marked blocks (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.want, tb.positions, cmp.AllowUnexported(tailPosition{})); diff != "" {
				t.Errorf("tracked positions (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantPin, tb.pin, cmp.AllowUnexported(tailPosition{})); diff != "" {
				t.Errorf("pin (-want, +got):\n%s", diff)
			}
		})
	}
}
