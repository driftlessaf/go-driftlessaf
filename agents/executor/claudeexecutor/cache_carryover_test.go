/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const carryoverMsg = "Prompt cache did not carry over from the previous turn"

// usage is the token usage a scripted turn reports.
type usage struct{ input, read, creation int64 }

// withUsage returns events with the message_start usage replaced by u.
func withUsage(events []string, u usage) []string {
	out := slices.Clone(events)
	out[0] = strings.Replace(out[0],
		`"usage":{"input_tokens":10,"output_tokens":1}`,
		fmt.Sprintf(`"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":1}`, u.input, u.read, u.creation), 1)
	return out
}

// carryoverRecords returns the carry-over log records written to logs.
func carryoverRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["msg"] == carryoverMsg {
			found = append(found, rec)
		}
	}
	return found
}

func newCarryoverExecutor(t *testing.T, client anthropic.Client, extra ...claudeexecutor.Option[errCapRequest, errCapResponse]) claudeexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	opts := append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](6),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
	}, extra...)
	exec, err := claudeexecutor.New(client, prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

func TestPromptCacheCarryoverLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		first usage
		next  usage
		opts  []claudeexecutor.Option[errCapRequest, errCapResponse]
		// want is the single expected log record's numeric fields; nil means
		// no record.
		want map[string]float64
	}{{
		name:  "second turn reads less than the first cached",
		first: usage{input: 10, creation: 8000},
		next:  usage{input: 10, read: 2000, creation: 6000},
		want: map[string]float64{
			"turn": 1, "previous_input_tokens": 8010, "baseline_input_tokens": 8000,
			"cache_read_tokens": 2000, "cache_creation_tokens": 6000, "lost_tokens": 6000,
		},
	}, {
		name:  "second turn reads the full prior prefix",
		first: usage{input: 10, creation: 8000},
		next:  usage{input: 10, read: 8000, creation: 200},
	}, {
		name:  "shrunk input still logs against the smaller total",
		first: usage{input: 10, creation: 19990},
		next:  usage{input: 10, read: 7000, creation: 2990},
		want: map[string]float64{
			"turn": 1, "previous_input_tokens": 20000, "baseline_input_tokens": 10000,
			"cache_read_tokens": 7000, "cache_creation_tokens": 2990, "lost_tokens": 3000,
		},
	}, {
		name:  "first cache write after a prompt below the minimum",
		first: usage{input: 900},
		next:  usage{input: 10, creation: 5000},
	}, {
		name:  "no cache usage on the second turn",
		first: usage{input: 10, creation: 8000},
		next:  usage{input: 8010},
	}, {
		name:  "loss under the threshold",
		first: usage{input: 10, creation: 8000},
		next:  usage{input: 10, read: 7500, creation: 500},
	}, {
		name:  "loss of exactly ten percent of a large prefix",
		first: usage{creation: 20000},
		next:  usage{input: 10, read: 18000, creation: 2010},
	}, {
		name:  "loss just over ten percent of a large prefix",
		first: usage{creation: 20000},
		next:  usage{input: 10, read: 17999, creation: 2011},
		want: map[string]float64{
			"turn": 1, "previous_input_tokens": 20000, "baseline_input_tokens": 20000,
			"cache_read_tokens": 17999, "cache_creation_tokens": 2011, "lost_tokens": 2001,
		},
	}, {
		name:  "cache control disabled",
		first: usage{input: 10, creation: 8000},
		next:  usage{input: 10, read: 2000, creation: 6000},
		opts:  []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithoutCacheControl[errCapRequest, errCapResponse]()},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var turns atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				var events []string
				if turns.Add(1) == 1 {
					events = withUsage(namedToolCallTurn(t, "msg_01", "toolu_g1", "git_grep", map[string]any{"ref": "main", "pattern": "x"}), tt.first)
				} else {
					events = withUsage(submitCallTurn(t, "msg_02", "toolu_s1", submitInput("done")), tt.next)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sseBody(t, events))
			}))
			t.Cleanup(srv.Close)

			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
			logs := &bytes.Buffer{}
			tools := map[string]claudetool.Metadata[errCapResponse]{"git_grep": gitGrepTool(nil)}
			if _, err := newCarryoverExecutor(t, client, tt.opts...).Execute(clogJSON(t, logs), errCapRequest{}, tools); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			recs := carryoverRecords(t, logs)
			if tt.want == nil {
				if len(recs) != 0 {
					t.Fatalf("carry-over records: got = %d, want = 0\n%s", len(recs), logs.String())
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("carry-over records: got = %d, want = 1\n%s", len(recs), logs.String())
			}
			for k, want := range tt.want {
				if got := recs[0][k]; got != want {
					t.Errorf("%s: got = %v, want = %v", k, got, want)
				}
			}
		})
	}
}

// A turn sent with a different tool_choice than the previous one is skipped:
// the text redirect forces the submit tool, which invalidates cached messages.
func TestPromptCacheCarryoverSkipsToolChoiceChange(t *testing.T) {
	t.Parallel()

	var turns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var events []string
		if turns.Add(1) == 1 {
			events = withUsage(fableTestTurn(t, 1, "", nil), usage{input: 10, creation: 8000})
		} else {
			events = withUsage(submitCallTurn(t, "msg_02", "toolu_s1", submitInput("done")), usage{input: 10, creation: 8010})
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	logs := &bytes.Buffer{}
	if _, err := newCarryoverExecutor(t, client).Execute(clogJSON(t, logs), errCapRequest{}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(logs.String(), "redirecting to terminal submission") {
		t.Fatalf("the text redirect never ran, so the skip is untested\n%s", logs.String())
	}
	if recs := carryoverRecords(t, logs); len(recs) != 0 {
		t.Fatalf("carry-over records: got = %d, want = 0\n%s", len(recs), logs.String())
	}
}

// A turn whose request the input budget rewrote is skipped, and only that
// turn: the next unreduced turn with cache loss still logs.
func TestPromptCacheCarryoverSkipsBudgetReducedTurn(t *testing.T) {
	t.Parallel()

	// Per-turn usage. Turn 2 is the reduced turn and loses its whole prefix;
	// turn 3 loses it again with nothing rewritten.
	turnUsage := []usage{
		{input: 10, creation: 5000},
		{input: 10, read: 5000, creation: 400},
		{input: 10, creation: 5410},
		{input: 10, creation: 5500},
	}

	var turns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			// A request counts over the budget until a result is reduced.
			s := string(body)
			tokens := 600 * strings.Count(s, `"type":"tool_result"`)
			if strings.Contains(s, "executor_reduced") {
				tokens = 700
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"input_tokens":%d}`, tokens)
		case "/v1/messages":
			n := int(turns.Add(1))
			var events []string
			switch n {
			case 1:
				events = namedToolCallTurn(t, "msg_01", "toolu_p1", "list_commits", map[string]any{"bytes": 6000})
			case 2, 3:
				events = namedToolCallTurn(t, fmt.Sprintf("msg_%02d", n), fmt.Sprintf("toolu_p%d", n), "list_commits", map[string]any{"bytes": 10})
			default:
				events = submitCallTurn(t, "msg_04", "toolu_s1", submitInput("done"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseBody(t, withUsage(events, turnUsage[n-1])))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	const maxTokens = 1000
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	exec := newCarryoverExecutor(t, client,
		claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](maxTokens),
		claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](windowFor(t, 1000, maxTokens)),
	)
	logs := &bytes.Buffer{}
	if _, err := exec.Execute(clogJSON(t, logs), errCapRequest{}, pageTools("pages")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(logs.String(), "Reduced older tool results to fit the model input budget") {
		t.Fatalf("the budget never reduced a result, so the skip is untested\n%s", logs.String())
	}
	recs := carryoverRecords(t, logs)
	if len(recs) != 1 {
		t.Fatalf("carry-over records: got = %d, want = 1 (turn 3 only)\n%s", len(recs), logs.String())
	}
	if got, want := recs[0]["turn"], float64(3); got != want {
		t.Errorf("turn: got = %v, want = %v", got, want)
	}
	if got, want := recs[0]["lost_tokens"], float64(5410); got != want {
		t.Errorf("lost_tokens: got = %v, want = %v", got, want)
	}
}
