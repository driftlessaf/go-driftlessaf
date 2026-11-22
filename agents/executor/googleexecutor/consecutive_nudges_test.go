/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"crypto/rand"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// maxTokensTurnJSON is a candidate the output token limit cut off mid answer.
// Its text part keeps the turn in the chat's curated history. The max-tokens
// nudge relies on that: unlike the other nudges, it does not replay the
// turn's input.
const maxTokensTurnJSON = `{
	"candidates":[{"content":{"role":"model","parts":[{"text":"cut off"}]},"finishReason":"MAX_TOKENS"}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

// TestConsecutiveNudges proves the nudge bound counts unusable turns in a
// row, not in total: a usable turn between them resets the count, and a run
// of more unusable turns than the bound fails with an error naming the last
// one's problem. The three unusable shapes share one count.
func TestConsecutiveNudges(t *testing.T) {
	const (
		emptyErr     = "unexpected response format from model (no parts in candidate) after 4 consecutive attempts"
		malformedErr = "unexpected response format from model (malformed function call) after 4 consecutive attempts"
		maxTokensErr = "unexpected response format from model (stopped at the output token limit of 8192) after 4 consecutive attempts"
	)
	// interleaved alternates n copies of bad with a lookup call, so no two
	// unusable turns are adjacent.
	interleaved := func(bad string, n int) []string {
		turns := make([]string, 0, 2*n)
		for range n {
			turns = append(turns, bad, lookupTurnJSON)
		}
		return turns
	}
	// usableBetweenRuns is two runs of three unusable turns, each at the
	// bound, split by one lookup call.
	usableBetweenRuns := func(bad string) []string {
		return slices.Concat(slices.Repeat([]string{bad}, 3), []string{lookupTurnJSON}, slices.Repeat([]string{bad}, 3))
	}

	for _, tc := range []struct {
		name string
		// turns scripts the response to each request in order. A request
		// past the end of turns gets the submit call.
		turns        []string
		wantErr      string
		wantRequests int32
	}{{
		name:         "empty turns between tool calls",
		turns:        interleaved(emptyTurnJSON, 5),
		wantRequests: 11,
	}, {
		name:         "malformed calls between tool calls",
		turns:        interleaved(malformedFunctionCallJSON, 5),
		wantRequests: 11,
	}, {
		name:         "cut-off turns between tool calls",
		turns:        interleaved(maxTokensTurnJSON, 5),
		wantRequests: 11,
	}, {
		name:         "two runs of empty turns at the bound",
		turns:        usableBetweenRuns(emptyTurnJSON),
		wantRequests: 8,
	}, {
		name:         "two runs of malformed calls at the bound",
		turns:        usableBetweenRuns(malformedFunctionCallJSON),
		wantRequests: 8,
	}, {
		name:         "two runs of cut-off turns at the bound",
		turns:        usableBetweenRuns(maxTokensTurnJSON),
		wantRequests: 8,
	}, {
		name:         "empty turns past the bound",
		turns:        slices.Repeat([]string{emptyTurnJSON}, 10),
		wantErr:      emptyErr,
		wantRequests: 4,
	}, {
		name:         "malformed calls past the bound",
		turns:        slices.Repeat([]string{malformedFunctionCallJSON}, 10),
		wantErr:      malformedErr,
		wantRequests: 4,
	}, {
		name:         "cut-off turns past the bound",
		turns:        slices.Repeat([]string{maxTokensTurnJSON}, 10),
		wantErr:      maxTokensErr,
		wantRequests: 4,
	}, {
		name:         "unusable turns past the bound after a tool call",
		turns:        slices.Concat([]string{lookupTurnJSON}, slices.Repeat([]string{malformedFunctionCallJSON}, 10)),
		wantErr:      malformedErr,
		wantRequests: 5,
	}, {
		name:         "mixed unusable turns share the bound",
		turns:        []string{malformedFunctionCallJSON, maxTokensTurnJSON, emptyTurnJSON, maxTokensTurnJSON, lookupTurnJSON},
		wantErr:      maxTokensErr,
		wantRequests: 4,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := newValidatingGenerateContentServer(t, nil, func(reqNum int, _ []byte) string {
				requests.Store(int32(reqNum))
				if reqNum <= len(tc.turns) {
					return tc.turns[reqNum-1]
				}
				return submitTurnJSON
			})
			// maxTurns sits far above every script, so the nudge bound, not
			// the turn limit, is what can stop the run.
			exec := newEmptyResponseExecutor(t, srv.URL, rand.Text(), 50)
			tools := map[string]googletool.Metadata[errCapResponse]{
				"lookup": {
					Definition: &genai.FunctionDeclaration{Name: "lookup"},
					Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
						return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"answer": "found"}}
					},
				},
			}

			resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Execute: got error %v, want success", err)
			case tc.wantErr == "" && resp.Answer != "42":
				t.Errorf("resp.Answer: got = %q, want = %q", resp.Answer, "42")
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Execute: got nil error, want %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("Execute error: got = %q, want it to contain %q", err, tc.wantErr)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Errorf("generateContent requests: got = %d, want = %d", got, tc.wantRequests)
			}
		})
	}
}
