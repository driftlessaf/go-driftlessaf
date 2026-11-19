/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
)

const (
	// contextServerBytesPerToken is the scripted provider's tokenizer: a
	// request counts one token per three bytes of its body.
	contextServerBytesPerToken = 3
	// contextPageBytes is the size of each scripted tool result.
	contextPageBytes = 16000
	// contextProtectedBytes is the page size the protect rows exempt.
	contextProtectedBytes = contextPageBytes + 1
)

// contextTokens is the scripted provider's count for a request body.
func contextTokens(body []byte) int64 {
	return int64((len(body) + contextServerBytesPerToken - 1) / contextServerBytesPerToken)
}

// newContextServer scripts a run of toolTurns turns that each call
// list_commits for a page of pageBytes(n) bytes, then a final answer. Each
// response reports the request's contextTokens as its input tokens, so the
// executor is anchored on a count that tracks what it actually sent. The
// response to request noUsageAt, counted from one, reports no input tokens;
// zero leaves every response counted.
func newContextServer(t *testing.T, toolTurns int, pageBytes func(n int) int, noUsageAt int) (anthropic.Client, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()

		assertRequestValid(t, n, body)
		inputTokens := contextTokens(body)
		if n == noUsageAt {
			inputTokens = 0
		}
		start := fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_%d","type":"message","role":"assistant","content":[],"model":"claude-haiku-5-5","stop_reason":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, n, inputTokens)
		events := []string{start}
		if n > toolTurns {
			events = append(events, answerTurn[1:]...)
		} else {
			events = append(events, toolUseBlockSSE(t, 0, fmt.Sprintf("toolu_%d", n), "list_commits", map[string]any{"bytes": pageBytes(n)})...)
			events = append(events,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`,
				`{"type":"message_stop"}`)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)
	return anthropic.NewClient(
			option.WithBaseURL(srv.URL),
			option.WithAPIKey("test"),
			option.WithMaxRetries(0),
		), func() [][]byte {
			mu.Lock()
			defer mu.Unlock()
			return append([][]byte(nil), bodies...)
		}
}

// contextRun runs the scripted conversation under opts and returns the
// request bodies the provider received.
func contextRun(t *testing.T, toolTurns int, pageBytes func(n int) int, opts ...claudeexecutor.Option[errCapRequest, errCapResponse]) [][]byte {
	t.Helper()
	return contextRunNoUsage(t, toolTurns, pageBytes, 0, opts...)
}

// contextRunNoUsage is contextRun with the response to request noUsageAt,
// counted from one, reporting no input tokens.
func contextRunNoUsage(t *testing.T, toolTurns int, pageBytes func(n int) int, noUsageAt int, opts ...claudeexecutor.Option[errCapRequest, errCapResponse]) [][]byte {
	t.Helper()
	client, bodies := newContextServer(t, toolTurns, pageBytes, noUsageAt)
	run := budgetRun{
		tools: pageTools("List commits."),
		opts: append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
			claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](toolTurns + 1),
		}, opts...),
	}
	if _, err := run.execute(t, client); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := bodies()
	if len(got) != toolTurns+1 {
		t.Fatalf("requests: got = %d, want = %d", len(got), toolTurns+1)
	}
	return got
}

func fixedPages(int) int { return contextPageBytes }

// cacheNeutralMessages returns each message of a request body as its JSON with
// every cache_control marker removed. The tail markers move every turn and
// do not change what the prompt cache matches.
func cacheNeutralMessages(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("messages request: %v", err)
	}
	out := make([]string, 0, len(req.Messages))
	for _, raw := range req.Messages {
		var m any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("message: %v", err)
		}
		stripCacheControl(m)
		clean, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("message: %v", err)
		}
		out = append(out, string(clean))
	}
	return out
}

// firstRewritten is the index of the first message of next that is not the
// same message of prev, or len(prev) when next only appends.
func firstRewritten(prev, next []string) int {
	for i := range prev {
		if i >= len(next) || prev[i] != next[i] {
			return i
		}
	}
	return len(prev)
}

// promptCache is a model of the provider's prompt cache, reduced to the two
// rules the executor's marker placement relies on: a request writes an entry
// for the prefix ending at each of its cache_control markers, and a marker
// reads the cache only when an earlier request wrote an entry for exactly its
// prefix. The provider also looks back about 20 blocks from a marker for an
// earlier entry; the model leaves that out, so it never credits a read the
// provider would not make.
type promptCache struct {
	entries map[[sha256.Size]byte]struct{}
}

// send records a request and returns the index of the newest message a
// marker read the cache through, or -1 when no marker inside the messages
// read it.
func (c *promptCache) send(t *testing.T, body []byte) int {
	t.Helper()
	var req struct {
		Tools    []any `json:"tools"`
		System   []any `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []any  `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("messages request: %v", err)
	}
	if c.entries == nil {
		c.entries = make(map[[sha256.Size]byte]struct{})
	}
	prefix := sha256.New()
	read := -1
	var written [][sha256.Size]byte
	// block folds one block into the prefix. message is the block's message
	// index, or -1 for the tools and the system prompt.
	block := func(message int, role string, v any) {
		marked := false
		if m, ok := v.(map[string]any); ok {
			_, marked = m["cache_control"]
		}
		stripCacheControl(v)
		clean, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("block: %v", err)
		}
		fmt.Fprintf(prefix, "%d %s %s\n", message, role, clean)
		if !marked {
			return
		}
		key := [sha256.Size]byte(prefix.Sum(nil))
		if _, hit := c.entries[key]; hit {
			read = message
		}
		written = append(written, key)
	}
	for _, v := range req.Tools {
		block(-1, "tool", v)
	}
	for _, v := range req.System {
		block(-1, "system", v)
	}
	for i, m := range req.Messages {
		for _, v := range m.Content {
			block(i, m.Role, v)
		}
	}
	for _, key := range written {
		c.entries[key] = struct{}{}
	}
	return read
}

// assertCacheReads pins what each request of a run reads from the prompt
// cache. A request that only appends reads through the previous request's
// last message. A compaction reads to within one turn of the first message
// it rewrote, and so does the request after it, which is the one that writes
// the messages the compaction left uncached.
func assertCacheReads(t *testing.T, bodies [][]byte, compactedAt map[int]claudeexecutor.ContextCompaction) {
	t.Helper()
	var cache promptCache
	cache.send(t, bodies[0])
	afterCompaction := -1
	for i := 1; i < len(bodies); i++ {
		prev, next := cacheNeutralMessages(t, bodies[i-1]), cacheNeutralMessages(t, bodies[i])
		read := cache.send(t, bodies[i])
		want := len(prev) - 1
		if _, compacted := compactedAt[i]; compacted {
			want = firstRewritten(prev, next) - 2
			afterCompaction = want
		} else if afterCompaction >= 0 {
			want, afterCompaction = afterCompaction, -1
		}
		if read < want {
			t.Errorf("request %d: the prompt cache was read through message %d, want at least message %d of %d", i+1, read, want, len(next))
		}
	}
}

// TestContextBudgetOffSendsIdenticalRequests pins that a run the budget does
// not act on sends byte-for-byte the requests a run without the option
// sends: the option unset, a zero budget, a budget never reached, and a
// budget passed with every result protected.
func TestContextBudgetOffSendsIdenticalRequests(t *testing.T) {
	t.Parallel()

	const toolTurns = 8
	want := contextRun(t, toolTurns, fixedPages)

	for _, tc := range []struct {
		name   string
		budget claudeexecutor.ContextBudget
	}{{
		name:   "zero budget",
		budget: claudeexecutor.ContextBudget{KeepRecentTurns: 2},
	}, {
		name:   "budget never reached",
		budget: claudeexecutor.ContextBudget{Tokens: 1_000_000, LowWaterTokens: 10},
	}, {
		name: "every result protected",
		budget: claudeexecutor.ContextBudget{
			Tokens:  20_000,
			Protect: func(string, json.RawMessage) bool { return true },
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			compactions := 0
			tc.budget.OnCompact = func(claudeexecutor.ContextCompaction) { compactions++ }
			got := contextRun(t, toolTurns, fixedPages, claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](tc.budget))
			if compactions != 0 {
				t.Errorf("compactions: got = %d, want = 0", compactions)
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Errorf("request %d differs from the run without the option (-want, +got):\n%s", i+1, cmp.Diff(string(want[i]), string(got[i])))
				}
			}
		})
	}
}

// compactionsByRequest indexes compactions by the request each rewrote,
// counted from zero as the recorded bodies are.
func compactionsByRequest(compactions []claudeexecutor.ContextCompaction) map[int]claudeexecutor.ContextCompaction {
	at := make(map[int]claudeexecutor.ContextCompaction, len(compactions))
	for _, c := range compactions {
		at[c.Turn] = c
	}
	return at
}

// TestContextBudgetLongRun drives a 20-turn run whose uncompacted prompt
// passes the budget several times over, and pins what the budget promises
// when shortening older results is enough: every request the provider counts
// is under it, compactions are few, each leaves the messages before the
// previous compaction untouched and reads the prompt cache up to the ones it
// rewrote, and every other turn only appends.
func TestContextBudgetLongRun(t *testing.T) {
	t.Parallel()

	const (
		toolTurns = 20
		budget    = 60_000
		keep      = 2
	)
	// The second page is the protected size, so one result is exempt.
	pages := func(n int) int {
		if n == 2 {
			return contextProtectedBytes
		}
		return contextPageBytes
	}

	baseline := contextRun(t, toolTurns, pages)
	if got := contextTokens(baseline[len(baseline)-1]); got <= budget {
		t.Fatalf("uncompacted final request: got = %d tokens, want over the %d budget for the test to mean anything", got, budget)
	}

	var compactions []claudeexecutor.ContextCompaction
	bodies := contextRun(t, toolTurns, pages, claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](claudeexecutor.ContextBudget{
		Tokens:          budget,
		KeepRecentTurns: keep,
		Protect: func(tool string, input json.RawMessage) bool {
			return tool == "list_commits" && strings.Contains(string(input), fmt.Sprint(contextProtectedBytes))
		},
		OnCompact: func(c claudeexecutor.ContextCompaction) { compactions = append(compactions, c) },
	}))

	for i, body := range bodies {
		if got := contextTokens(body); got > budget {
			t.Errorf("request %d: got = %d tokens, want at most the %d budget", i+1, got, budget)
		}
	}
	if n := len(compactions); n == 0 || n > toolTurns/5 {
		t.Errorf("compactions: got = %d, want between 1 and %d", n, toolTurns/5)
	}

	compactedAt := compactionsByRequest(compactions)
	for _, c := range compactions {
		if c.EstimatedTokens <= budget || c.ReducedResults == 0 || c.RecentResults != 0 || c.SavedBytes <= 0 {
			t.Errorf("compaction %+v: want an estimate over the budget, a saving and no recent result", c)
		}
	}
	// boundary is the first message the latest compaction rewrote.
	boundary := 0
	for i := 1; i < len(bodies); i++ {
		prev, next := cacheNeutralMessages(t, bodies[i-1]), cacheNeutralMessages(t, bodies[i])
		rewritten := firstRewritten(prev, next)
		c, compacted := compactedAt[i]
		if !compacted {
			if rewritten != len(prev) {
				t.Errorf("request %d: message %d was rewritten on a turn with no compaction", i+1, rewritten)
			}
			continue
		}
		if rewritten <= boundary && boundary > 0 {
			t.Errorf("request %d: compaction rewrote message %d, at or before the previous compaction's first rewritten message %d", i+1, rewritten, boundary)
		}
		boundary = rewritten

		// The newest result and the keep window stay whole, and so does the
		// protected page.
		for n := i - keep; n <= i; n++ {
			if resultReduced(t, bodies[i], fmt.Sprintf("toolu_%d", n)) {
				t.Errorf("request %d: toolu_%d is inside the keep window and was shortened", i+1, n)
			}
		}
		if resultReduced(t, bodies[i], "toolu_2") {
			t.Errorf("request %d: the protected toolu_2 was shortened", i+1)
		}
		if !resultReduced(t, bodies[i], fmt.Sprintf("toolu_%d", i-keep-1)) {
			t.Errorf("request %d (compaction %+v): toolu_%d is older than the keep window and was not shortened", i+1, c, i-keep-1)
		}
	}
	assertCacheReads(t, bodies, compactedAt)
}

// TestContextBudgetOutOfReach pins a run whose budget no shortening can
// meet: no request fails, and every request carries every result the model
// has already read shortened, keep window included, with only the newest
// one whole.
func TestContextBudgetOutOfReach(t *testing.T) {
	t.Parallel()

	const toolTurns = 8
	baseline := contextRun(t, toolTurns, fixedPages)

	var compactions []claudeexecutor.ContextCompaction
	bodies := contextRun(t, toolTurns, fixedPages, claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](claudeexecutor.ContextBudget{
		Tokens:          100,
		KeepRecentTurns: 2,
		OnCompact:       func(c claudeexecutor.ContextCompaction) { compactions = append(compactions, c) },
	}))

	// The first request has no result, and the second has only an unread one.
	for i := 2; i < len(bodies); i++ {
		if got, uncompacted := len(bodies[i]), len(baseline[i]); got >= uncompacted {
			t.Errorf("request %d: got = %d bytes, want fewer than the %d of the run without a budget", i+1, got, uncompacted)
		}
		for n := 1; n < i; n++ {
			if !resultReduced(t, bodies[i], fmt.Sprintf("toolu_%d", n)) {
				t.Errorf("request %d: toolu_%d has been read and was not shortened", i+1, n)
			}
		}
		if resultReduced(t, bodies[i], fmt.Sprintf("toolu_%d", i)) {
			t.Errorf("request %d: the unread toolu_%d was shortened", i+1, i)
		}
	}
	if got, want := len(compactions), toolTurns-1; got != want {
		t.Errorf("compactions: got = %d, want = %d, one for each request with a read result", got, want)
	}
	assertCacheReads(t, bodies, compactionsByRequest(compactions))
}

// TestContextBudgetOversizedResult pins what one result larger than the
// budget costs. The request that delivers it carries it whole. The next
// request shortens it although it is inside the keep window, and leaves the
// window's other results whole.
func TestContextBudgetOversizedResult(t *testing.T) {
	t.Parallel()

	const (
		toolTurns = 10
		budget    = 80_000
		keep      = 3
		oversized = 6
	)
	pages := func(n int) int {
		if n == oversized {
			return 25 * contextPageBytes
		}
		return contextPageBytes
	}

	var compactions []claudeexecutor.ContextCompaction
	bodies := contextRun(t, toolTurns, pages, claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](claudeexecutor.ContextBudget{
		Tokens:          budget,
		KeepRecentTurns: keep,
		OnCompact:       func(c claudeexecutor.ContextCompaction) { compactions = append(compactions, c) },
	}))
	big := fmt.Sprintf("toolu_%d", oversized)

	// bodies[oversized] is the request that delivers the oversized result.
	delivering, after := bodies[oversized], bodies[oversized+1]
	if resultReduced(t, delivering, big) {
		t.Errorf("request %d: the unread %s was shortened", oversized+1, big)
	}
	if got := contextTokens(delivering); got <= budget {
		t.Fatalf("request %d: got = %d tokens, want over the %d budget for the test to mean anything", oversized+1, got, budget)
	}
	if !resultReduced(t, after, big) {
		t.Errorf("request %d: %s has been read and was not shortened", oversized+2, big)
	}
	for n := oversized - keep + 1; n <= oversized+1; n++ {
		if n != oversized && resultReduced(t, after, fmt.Sprintf("toolu_%d", n)) {
			t.Errorf("request %d: toolu_%d was shortened, want only %s taken from the keep window", oversized+2, n, big)
		}
	}
	for i, body := range bodies {
		if got := contextTokens(body); i != oversized && got > budget {
			t.Errorf("request %d: got = %d tokens, want at most the %d budget", i+1, got, budget)
		}
	}
	compactedAt := compactionsByRequest(compactions)
	if c := compactedAt[oversized+1]; c.RecentResults != 1 {
		t.Errorf("compaction of request %d: got = %+v, want one recent result", oversized+2, c)
	}
	assertCacheReads(t, bodies, compactedAt)
}

// TestContextBudgetLowWater pins that a low-water mark under the budget
// makes compactions rarer, and that each one brings the request the
// provider counts down to the mark.
func TestContextBudgetLowWater(t *testing.T) {
	t.Parallel()

	const (
		toolTurns = 20
		budget    = 60_000
		lowWater  = 35_000
		keep      = 4
	)
	run := func(lowWater int64) ([][]byte, []claudeexecutor.ContextCompaction) {
		var compactions []claudeexecutor.ContextCompaction
		bodies := contextRun(t, toolTurns, fixedPages, claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](claudeexecutor.ContextBudget{
			Tokens:          budget,
			LowWaterTokens:  lowWater,
			KeepRecentTurns: keep,
			OnCompact:       func(c claudeexecutor.ContextCompaction) { compactions = append(compactions, c) },
		}))
		return bodies, compactions
	}

	_, atBudget := run(0)
	bodies, atMark := run(lowWater)
	if len(atMark) == 0 || len(atMark) >= len(atBudget) {
		t.Errorf("compactions: got = %d with the low-water mark and %d without, want fewer with it", len(atMark), len(atBudget))
	}
	for _, c := range atMark {
		if got := contextTokens(bodies[c.Turn]); got > lowWater {
			t.Errorf("request %d: got = %d tokens after its compaction, want at most the %d low-water mark", c.Turn+1, got, lowWater)
		}
	}
	for i, body := range bodies {
		if got := contextTokens(body); got > budget {
			t.Errorf("request %d: got = %d tokens, want at most the %d budget", i+1, got, budget)
		}
	}
	assertCacheReads(t, bodies, compactionsByRequest(atMark))
}

// TestContextBudgetWithoutTailCacheControl pins that a compaction places no
// message markers when the caller turned the tail markers off: nothing would
// clear them, so every later request would pay the cache-write premium.
func TestContextBudgetWithoutTailCacheControl(t *testing.T) {
	t.Parallel()

	var compactions int
	bodies := contextRun(t, 20, fixedPages,
		claudeexecutor.WithoutTailCacheControl[errCapRequest, errCapResponse](),
		claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](claudeexecutor.ContextBudget{
			Tokens:          60_000,
			LowWaterTokens:  35_000,
			KeepRecentTurns: 4,
			OnCompact:       func(claudeexecutor.ContextCompaction) { compactions++ },
		}))
	if compactions == 0 {
		t.Fatal("compactions: got = 0, want at least one")
	}
	for i, body := range bodies {
		var req struct {
			Messages []struct {
				Content []map[string]any `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		for m, msg := range req.Messages {
			for b, block := range msg.Content {
				if _, marked := block["cache_control"]; marked {
					t.Errorf("request %d message %d block %d: got a cache_control marker, want none", i+1, m, b)
				}
			}
		}
	}
}

// TestContextBudgetMissingUsage pins that a response reporting no prompt
// usage leaves the earlier provider count in place. Its output tokens alone
// would estimate the next request far under the budget, and a conversation
// that keeps growing would skip the compaction it is due.
func TestContextBudgetMissingUsage(t *testing.T) {
	t.Parallel()

	const toolTurns = 20
	run := func(noUsageAt int) []claudeexecutor.ContextCompaction {
		var compactions []claudeexecutor.ContextCompaction
		contextRunNoUsage(t, toolTurns, fixedPages, noUsageAt, claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](claudeexecutor.ContextBudget{
			Tokens:          60_000,
			KeepRecentTurns: 2,
			OnCompact:       func(c claudeexecutor.ContextCompaction) { compactions = append(compactions, c) },
		}))
		return compactions
	}

	counted := run(0)
	if len(counted) == 0 {
		t.Fatal("compactions with every response counted: got = 0, want at least one")
	}
	// first is the first request compacted, counted from zero, so the
	// response before it answers request first counted from one.
	first := counted[0].Turn
	if first < 2 {
		t.Fatalf("first compacted request: got = %d, want a counted request before the uncounted one", first)
	}

	uncounted := run(first)
	if len(uncounted) == 0 {
		t.Fatal("compactions with one response uncounted: got = 0, want at least one")
	}
	if got := uncounted[0]; got.Turn != first || got.EstimatedTokens < counted[0].EstimatedTokens {
		t.Errorf("first compaction: got = %+v, want request %d estimated at no less than %d tokens", got, first, counted[0].EstimatedTokens)
	}
}

// TestContextBudgetRejectsInvalid pins the option's validation.
func TestContextBudgetRejectsInvalid(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		budget  claudeexecutor.ContextBudget
		wantErr string
	}{{
		name:    "negative tokens",
		budget:  claudeexecutor.ContextBudget{Tokens: -1},
		wantErr: "context budget tokens must not be negative",
	}, {
		name:    "negative keep window",
		budget:  claudeexecutor.ContextBudget{Tokens: 10, KeepRecentTurns: -1},
		wantErr: "context budget keep-recent turns must not be negative",
	}, {
		name:    "negative low-water mark",
		budget:  claudeexecutor.ContextBudget{Tokens: 10, LowWaterTokens: -1},
		wantErr: "context budget low-water tokens must be between 0 and the 10-token budget",
	}, {
		name:    "low-water mark over the budget",
		budget:  claudeexecutor.ContextBudget{Tokens: 10, LowWaterTokens: 11},
		wantErr: "context budget low-water tokens must be between 0 and the 10-token budget",
	}, {
		name:    "low-water mark without a budget",
		budget:  claudeexecutor.ContextBudget{LowWaterTokens: 5},
		wantErr: "context budget low-water tokens must be between 0 and the 0-token budget",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newContextServer(t, 0, fixedPages, 0)
			_, err := claudeexecutor.New(client, boundPrompt(t, "Answer the question."),
				claudeexecutor.WithContextBudget[errCapRequest, errCapResponse](tc.budget))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("New: got = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
