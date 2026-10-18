/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"google.golang.org/genai"
)

// resultCase is one request in a job and the result it must produce. line
// builds its output line from the key's reference and the request line as
// written; a nil line means Vertex wrote nothing for it.
type resultCase struct {
	name    string
	line    func(ref string, input map[string]any) string
	want    Result
	wantErr error
	// wantLineErr expects a per-line provider error, transient or not.
	wantLineErr   bool
	wantRetryable bool
}

// collect runs Results to completion and indexes the outcome by key.
func collect(t *testing.T, c *Client, job Job) map[string]Result {
	t.Helper()
	got := map[string]Result{}
	for res, err := range c.Results(t.Context(), job) {
		if err != nil {
			t.Fatalf("Results: %v", err)
		}
		if _, dup := got[res.Key]; dup {
			t.Errorf("key %q yielded twice", res.Key)
		}
		got[res.Key] = res
	}
	return got
}

func checkResults(t *testing.T, cases []resultCase, keys []string, got map[string]Result) {
	t.Helper()
	if len(got) != len(cases) {
		t.Errorf("results: got = %d, want = %d", len(got), len(cases))
	}
	for i, tc := range cases {
		res, ok := got[keys[i]]
		if !ok {
			t.Errorf("%s: no result", tc.name)
			continue
		}
		switch {
		case tc.wantErr != nil:
			if !errors.Is(res.Err, tc.wantErr) {
				t.Errorf("%s: err: got = %v, want = %v", tc.name, res.Err, tc.wantErr)
			}
		case tc.wantLineErr:
			if res.Err == nil || errors.Is(res.Err, ErrRefused) || errors.Is(res.Err, ErrTruncated) || errors.Is(res.Err, ErrMissing) {
				t.Errorf("%s: err: got = %v, want a line error", tc.name, res.Err)
			}
			if got := IsRetryable(res.Err); got != tc.wantRetryable {
				t.Errorf("%s: IsRetryable(%v): got = %v, want = %v", tc.name, res.Err, got, tc.wantRetryable)
			}
		case res.Err != nil:
			t.Errorf("%s: err: got = %v, want nil", tc.name, res.Err)
		}
		tc.want.Key = keys[i]
		if diff := cmp.Diff(tc.want, res, cmp.FilterPath(func(p cmp.Path) bool { return p.Last().String() == ".Err" }, cmp.Ignore())); diff != "" {
			t.Errorf("%s (-want, +got):\n%s", tc.name, diff)
		}
	}
}

func claudeMessage(text, stop, stopDetails string) string {
	details := "null"
	if stopDetails != "" {
		details = stopDetails
	}
	return fmt.Sprintf(`{"id":"msg_%s","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"thinking","thinking":"hidden","signature":"s"},{"type":"text","text":%q}],"stop_reason":%q,"stop_details":%s,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}`,
		uuid.NewString(), text, stop, details)
}

func TestResultsClaude(t *testing.T) {
	t.Parallel()
	usage := Usage{In: 100, Out: 20, CacheRead: 7, CacheWrite: 3}
	cases := []resultCase{{
		name: "vertex envelope",
		line: func(ref string, in map[string]any) string {
			req, _ := json.Marshal(in["request"])
			return fmt.Sprintf(`{"custom_id":%q,"request":%s,"response":%s,"processed_time":"2026-10-18T01:00:00Z"}`, ref, req, claudeMessage("verdict: ok", "end_turn", ""))
		},
		want: Result{Text: "verdict: ok", Stop: "end_turn", Usage: usage},
	}, {
		name: "anthropic envelope",
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"result":{"type":"succeeded","message":%s}}`, ref, claudeMessage("fine", "end_turn", ""))
		},
		want: Result{Text: "fine", Stop: "end_turn", Usage: usage},
	}, {
		name: "refusal keeps category and explanation",
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref, claudeMessage("", "refusal",
				`{"type":"refusal","category":"reasoning_extraction","explanation":"asks for hidden reasoning"}`))
		},
		want:    Result{Stop: "refusal", Usage: usage, Refusal: &Refusal{Category: "reasoning_extraction", Detail: "asks for hidden reasoning"}},
		wantErr: ErrRefused,
	}, {
		name: "refusal with null category",
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref, claudeMessage("", "refusal", `{"type":"refusal","category":null,"explanation":null}`))
		},
		want:    Result{Stop: "refusal", Usage: usage, Refusal: &Refusal{}},
		wantErr: ErrRefused,
	}, {
		name: "max tokens",
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref, claudeMessage("partial", "max_tokens", ""))
		},
		want:    Result{Text: "partial", Stop: "max_tokens", Usage: usage},
		wantErr: ErrTruncated,
	}, {
		name: "end turn with no text",
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"response":{"id":"m","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":0}}}`, ref)
		},
		want:          Result{Stop: "end_turn", Usage: Usage{In: 10}},
		wantLineErr:   true,
		wantRetryable: true,
	}, {
		name: "overloaded line error",
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"result":{"type":"errored","error":{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}}}`, ref)
		},
		wantLineErr:   true,
		wantRetryable: true,
	}, {
		name: "expired row",
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"result":{"type":"expired"}}`, ref)
		},
		wantLineErr:   true,
		wantRetryable: true,
	}, {
		name:        "invalid request line error",
		wantLineErr: true,
		line: func(ref string, _ map[string]any) string {
			return fmt.Sprintf(`{"custom_id":%q,"status":"invalid_request_error: prompt is too long"}`, ref)
		},
	}, {
		name:    "no line",
		wantErr: ErrMissing,
	}}
	for _, state := range []genai.JobState{genai.JobStatePartiallySucceeded, genai.JobStateExpired} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			keys := make([]string, len(cases))
			reqs := make([]ClaudeRequest, len(cases))
			for i := range cases {
				keys[i] = fmt.Sprintf("record/%d/%s", i, uuid.NewString())
				reqs[i] = claudeReq(keys[i], "prompt "+keys[i])
			}
			job, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default", reqs)
			if err != nil {
				t.Fatalf("SubmitClaude: %v", err)
			}
			data, _ := env.gcs.get(testPrefix + "/" + job.ID + "/input.jsonl")
			inputs := jsonLines(t, data)
			var lines []string
			for i, tc := range cases {
				if tc.line != nil {
					lines = append(lines, tc.line(ref(keys[i]), inputs[i]))
				}
			}
			// A stray line from no submitted key is skipped.
			lines = append(lines, fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref("stranger"), claudeMessage("x", "end_turn", "")))
			env.vertex.finish(t, env.gcs, job.Name, state, lines...)
			checkResults(t, cases, keys, collect(t, env.client, job))
		})
	}
}

func geminiResponse(parts, finish, extra string) string {
	return fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[%s]},"finishReason":%q%s}],"usageMetadata":{"promptTokenCount":50,"toolUsePromptTokenCount":5,"candidatesTokenCount":10,"thoughtsTokenCount":4,"cachedContentTokenCount":30,"totalTokenCount":69}}`, parts, finish, extra)
}

// echoLabels selects which labels Vertex echoes on a request.
type echoLabels int

const (
	echoAllLabels echoLabels = iota
	echoCallerLabels
	echoNoLabels
)

// echo renders a request as Vertex might echo it: field order, spacing, and
// which labels come back may all differ from what was sent.
func echo(t *testing.T, in map[string]any, labels echoLabels) string {
	t.Helper()
	req := maps.Clone(in["request"].(map[string]any))
	switch labels {
	case echoCallerLabels:
		caller := maps.Clone(req["labels"].(map[string]any))
		delete(caller, keyLabel)
		req["labels"] = caller
	case echoNoLabels:
		delete(req, "labels")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), `,"`, `, "`)
}

func TestResultsGemini(t *testing.T) {
	t.Parallel()
	usage := Usage{In: 55, Out: 14, CacheRead: 30}
	ok := geminiResponse(`{"text":"thinking","thought":true},{"text":"verdict: "},{"text":"ok"}`, "STOP", "")
	cases := []resultCase{{
		name: "label echoed",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"status":"","processed_time":"2026-10-18T01:00:00Z","request":%s,"response":%s}`, echo(t, in, echoAllLabels), ok)
		},
		want: Result{Text: "verdict: ok", Stop: "STOP", Usage: usage},
	}, {
		name: "label dropped, matched by digest",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoCallerLabels), ok)
		},
		want: Result{Text: "verdict: ok", Stop: "STOP", Usage: usage},
	}, {
		name: "no labels echoed, matched by digest",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoNoLabels), ok)
		},
		want: Result{Text: "verdict: ok", Stop: "STOP", Usage: usage},
	}, {
		name: "unspecified block reason is not a block",
		line: func(_ string, in map[string]any) string {
			resp := strings.Replace(ok, `{"candidates"`, `{"promptFeedback":{"blockReason":"BLOCK_REASON_UNSPECIFIED"},"candidates"`, 1)
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), resp)
		},
		want: Result{Text: "verdict: ok", Stop: "STOP", Usage: usage},
	}, {
		name: "unspecified block reason with no candidates",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":{"promptFeedback":{"blockReason":"BLOCKED_REASON_UNSPECIFIED"},"usageMetadata":{"promptTokenCount":50}}}`, echo(t, in, echoAllLabels))
		},
		want:          Result{Usage: Usage{In: 50}},
		wantLineErr:   true,
		wantRetryable: true,
	}, {
		name: "prompt blocked",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT","blockReasonMessage":"blocked by policy"},"usageMetadata":{"promptTokenCount":50}}}`, echo(t, in, echoAllLabels))
		},
		want:    Result{Stop: "PROHIBITED_CONTENT", Usage: Usage{In: 50}, Refusal: &Refusal{Category: "PROHIBITED_CONTENT", Detail: "blocked by policy"}},
		wantErr: ErrRefused,
	}, {
		name: "safety finish",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), geminiResponse(`{"text":"par"}`, "SAFETY", `,"finishMessage":"unsafe content"`))
		},
		want:    Result{Text: "par", Stop: "SAFETY", Usage: usage, Refusal: &Refusal{Category: "SAFETY", Detail: "unsafe content"}},
		wantErr: ErrRefused,
	}, {
		name: "other finish fails closed",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), geminiResponse(`{"text":"partial"}`, "OTHER", ""))
		},
		want:    Result{Text: "partial", Stop: "OTHER", Usage: usage, Refusal: &Refusal{Category: "OTHER"}},
		wantErr: ErrRefused,
	}, {
		name: "malformed function call fails closed",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), geminiResponse(`{"text":"partial"}`, "MALFORMED_FUNCTION_CALL", ""))
		},
		want:    Result{Text: "partial", Stop: "MALFORMED_FUNCTION_CALL", Usage: usage, Refusal: &Refusal{Category: "MALFORMED_FUNCTION_CALL"}},
		wantErr: ErrRefused,
	}, {
		name: "unspecified finish fails closed",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), geminiResponse(`{"text":"partial"}`, "FINISH_REASON_UNSPECIFIED", ""))
		},
		want:    Result{Text: "partial", Stop: "FINISH_REASON_UNSPECIFIED", Usage: usage, Refusal: &Refusal{Category: "FINISH_REASON_UNSPECIFIED"}},
		wantErr: ErrRefused,
	}, {
		name: "unknown finish fails closed",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), geminiResponse(`{"text":"partial"}`, "SOME_FUTURE_REASON", `,"finishMessage":"new"`))
		},
		want:    Result{Text: "partial", Stop: "SOME_FUTURE_REASON", Usage: usage, Refusal: &Refusal{Category: "SOME_FUTURE_REASON", Detail: "new"}},
		wantErr: ErrRefused,
	}, {
		name: "stop with no text",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), geminiResponse(``, "STOP", ""))
		},
		want:          Result{Stop: "STOP", Usage: usage},
		wantLineErr:   true,
		wantRetryable: true,
	}, {
		name: "max tokens",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, in, echoAllLabels), geminiResponse(`{"text":"half"}`, "MAX_TOKENS", ""))
		},
		want:    Result{Text: "half", Stop: "MAX_TOKENS", Usage: usage},
		wantErr: ErrTruncated,
	}, {
		name: "quota line error",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"status":"RESOURCE_EXHAUSTED: quota exceeded for model","request":%s}`, echo(t, in, echoAllLabels))
		},
		wantLineErr:   true,
		wantRetryable: true,
	}, {
		name: "no candidates",
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"request":%s,"response":{"candidates":[],"usageMetadata":{"promptTokenCount":50}}}`, echo(t, in, echoAllLabels))
		},
		want:          Result{Usage: Usage{In: 50}},
		wantLineErr:   true,
		wantRetryable: true,
	}, {
		name:        "invalid argument line error",
		wantLineErr: true,
		line: func(_ string, in map[string]any) string {
			return fmt.Sprintf(`{"status":{"code":400,"message":"INVALID_ARGUMENT"},"request":%s}`, echo(t, in, echoAllLabels))
		},
	}, {
		name:    "no line",
		wantErr: ErrMissing,
	}}
	env := newTestEnv(t)
	keys := make([]string, len(cases))
	reqs := make([]GeminiRequest, len(cases))
	for i := range cases {
		keys[i] = fmt.Sprintf("record/%d/%s", i, uuid.NewString())
		reqs[i] = geminiReq(keys[i], "prompt "+keys[i])
	}
	job, err := env.client.SubmitGemini(t.Context(), uuid.NewString(), "gemini-3.8-flash", reqs)
	if err != nil {
		t.Fatalf("SubmitGemini: %v", err)
	}
	data, _ := env.gcs.get(testPrefix + "/" + job.ID + "/input.jsonl")
	inputs := jsonLines(t, data)
	var lines []string
	for i, tc := range cases {
		if tc.line != nil {
			lines = append(lines, tc.line(ref(keys[i]), inputs[i]))
		}
	}
	env.vertex.finish(t, env.gcs, job.Name, genai.JobStateSucceeded, lines...)
	checkResults(t, cases, keys, collect(t, env.client, job))
}

// TestResultsGeminiIdenticalRequests covers the digest fallback when two keys
// sent the same request and Vertex echoed no label: each key gets one result.
func TestResultsGeminiIdenticalRequests(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	keys := []string{uuid.NewString(), uuid.NewString()}
	job, err := env.client.SubmitGemini(t.Context(), uuid.NewString(), "gemini-3.8-flash",
		[]GeminiRequest{geminiReq(keys[0], "same"), geminiReq(keys[1], "same")})
	if err != nil {
		t.Fatalf("SubmitGemini: %v", err)
	}
	data, _ := env.gcs.get(testPrefix + "/" + job.ID + "/input.jsonl")
	inputs := jsonLines(t, data)
	resp := geminiResponse(`{"text":"same answer"}`, "STOP", "")
	env.vertex.finish(t, env.gcs, job.Name, genai.JobStateSucceeded,
		fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, inputs[0], echoCallerLabels), resp),
		fmt.Sprintf(`{"request":%s,"response":%s}`, echo(t, inputs[1], echoCallerLabels), resp))
	got := collect(t, env.client, job)
	for _, k := range keys {
		if res := got[k]; res.Err != nil || res.Text != "same answer" {
			t.Errorf("key %q: got = %+v", k, res)
		}
	}
}

func TestResultsBeforeTerminal(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	job, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
	if err != nil {
		t.Fatalf("SubmitClaude: %v", err)
	}
	if got := collect(t, env.client, job); len(got) != 0 {
		t.Errorf("results before terminal: got = %v, want none", got)
	}
}

func TestResultsRetriesReadsWithoutRepeats(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	keys := []string{uuid.NewString(), uuid.NewString()}
	job, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default",
		[]ClaudeRequest{claudeReq(keys[0], "a"), claudeReq(keys[1], "b")})
	if err != nil {
		t.Fatalf("SubmitClaude: %v", err)
	}
	env.vertex.finish(t, env.gcs, job.Name, genai.JobStateSucceeded,
		fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref(keys[0]), claudeMessage("a", "end_turn", "")),
		fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref(keys[1]), claudeMessage("b", "end_turn", "")))
	env.gcs.mu.Lock()
	// The manifest read and the first output read each fail once.
	env.gcs.failReads = 2
	env.gcs.mu.Unlock()
	got := collect(t, env.client, job)
	if len(got) != 2 || got[keys[0]].Text != "a" || got[keys[1]].Text != "b" {
		t.Errorf("results: got = %+v", got)
	}
}

func TestResultsRejectsForeignOutputDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// tamper points the job's Vertex output elsewhere or edits the
		// caller-held Job; other is a finished job with output of its own.
		tamper func(env *testEnv, job, other *Job)
	}{{
		name: "output outside the job's directory",
		tamper: func(env *testEnv, job, _ *Job) {
			env.vertex.setOutputDir(job.Name, "gs://"+testBucket+"/elsewhere/prediction-model-x")
		},
	}, {
		name: "another job's output with a widened output prefix",
		tamper: func(env *testEnv, job, other *Job) {
			env.vertex.setOutputDir(job.Name, env.vertex.outputDir(other.Name))
			job.OutputPrefix = "gs://" + testBucket + "/"
		},
	}, {
		name: "id escaping the prefix",
		tamper: func(_ *testEnv, job, other *Job) {
			job.ID = "../" + other.ID
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			key := uuid.NewString()
			job, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default", []ClaudeRequest{claudeReq(key, "p")})
			if err != nil {
				t.Fatalf("SubmitClaude: %v", err)
			}
			other, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default", []ClaudeRequest{claudeReq(key, "p")})
			if err != nil {
				t.Fatalf("SubmitClaude: %v", err)
			}
			env.vertex.finish(t, env.gcs, job.Name, genai.JobStateSucceeded)
			env.vertex.finish(t, env.gcs, other.Name, genai.JobStateSucceeded,
				fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref(key), claudeMessage("from the other job", "end_turn", "")))
			tt.tamper(env, &job, &other)
			var errs int
			for res, err := range env.client.Results(t.Context(), job) {
				if err != nil {
					errs++
					continue
				}
				t.Errorf("result: got = %+v, want none", res)
			}
			if errs != 1 {
				t.Errorf("errors: got = %d, want = 1", errs)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state    genai.JobState
		terminal bool
	}{
		{genai.JobStateQueued, false},
		{genai.JobStatePending, false},
		{genai.JobStateRunning, false},
		{genai.JobStateCancelling, false},
		{genai.JobStateUpdating, false},
		{genai.JobStateSucceeded, true},
		{genai.JobStatePartiallySucceeded, true},
		{genai.JobStateFailed, true},
		{genai.JobStateCancelled, true},
		{genai.JobStateExpired, true},
		{genai.JobStatePaused, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			job, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
			if err != nil {
				t.Fatalf("SubmitClaude: %v", err)
			}
			env.vertex.finish(t, env.gcs, job.Name, tt.state)
			got, err := env.client.Status(t.Context(), job)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			want := Status{State: tt.state, Terminal: tt.terminal}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Status (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestCancel(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	job, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
	if err != nil {
		t.Fatalf("SubmitClaude: %v", err)
	}
	if err := env.client.Cancel(t.Context(), job); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	st, err := env.client.Status(t.Context(), job)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != genai.JobStateCancelling || st.Terminal {
		t.Errorf("status after cancel: got = %+v", st)
	}
	if err := env.client.Cancel(t.Context(), Job{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("Cancel(Job{}): got = %v, want = %v", err, ErrInvalidRequest)
	}
}

func TestFindNotFound(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	if _, ok, err := env.client.Find(t.Context(), uuid.NewString()); ok || err != nil {
		t.Errorf("Find: ok = %v, err = %v, want not found", ok, err)
	}
}
