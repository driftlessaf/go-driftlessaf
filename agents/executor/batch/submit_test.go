/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"google.golang.org/genai"
)

func claudeReq(key, prompt string) ClaudeRequest {
	return ClaudeRequest{Key: key, Params: anthropic.MessageNewParams{
		Model:     "ignored-model",
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: "You review skills."}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	}}
}

func geminiReq(key, prompt string) GeminiRequest {
	return GeminiRequest{
		Key:      key,
		Contents: []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("You review skills.", genai.RoleUser),
			Temperature:       new(float32(0.2)),
			MaxOutputTokens:   2048,
			Labels:            map[string]string{"team": "skills"},
			HTTPOptions:       &genai.HTTPOptions{Headers: map[string][]string{"X-Secret": {"never-sent"}}},
		},
	}
}

// TestSubmitModelPath pins the model path genai produces from the bare IDs
// callers pass: genai's publisher mapping is the contract the package relies
// on, so a change in it must fail here.
func TestSubmitModelPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		submit func(*Client, string) (Job, error)
		want   string
	}{{
		name: "claude",
		submit: func(c *Client, id string) (Job, error) {
			return c.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
		},
		want: "publishers/anthropic/models/claude-opus-5-5@default",
	}, {
		name: "gemini",
		submit: func(c *Client, id string) (Job, error) {
			return c.SubmitGemini(t.Context(), id, "gemini-3.8-flash", []GeminiRequest{geminiReq("k", "p")})
		},
		want: "publishers/google/models/gemini-3.8-flash",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			id := uuid.NewString()
			job, err := tt.submit(env.client, id)
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			if len(env.vertex.creates) != 1 {
				t.Fatalf("creates: got = %d, want = 1", len(env.vertex.creates))
			}
			body := env.vertex.creates[0]
			if got := body["model"]; got != tt.want {
				t.Errorf("model: got = %v, want = %q", got, tt.want)
			}
			if got := body["displayName"]; got != id {
				t.Errorf("displayName: got = %v, want = %q", got, id)
			}
			wantIn := "gs://" + testBucket + "/" + testPrefix + "/" + id + "/input.jsonl"
			wantOut := "gs://" + testBucket + "/" + testPrefix + "/" + id + "/output/"
			wantBody := map[string]any{
				"inputConfig":  map[string]any{"instancesFormat": "jsonl", "gcsSource": map[string]any{"uris": []any{wantIn}}},
				"outputConfig": map[string]any{"predictionsFormat": "jsonl", "gcsDestination": map[string]any{"outputUriPrefix": wantOut}},
			}
			gotBody := map[string]any{"inputConfig": body["inputConfig"], "outputConfig": body["outputConfig"]}
			if diff := cmp.Diff(wantBody, gotBody); diff != "" {
				t.Errorf("job config (-want, +got):\n%s", diff)
			}
			if job.InputURI != wantIn || job.OutputPrefix != wantOut || job.Keys != 1 || job.ID != id || job.Name == "" {
				t.Errorf("job: got = %+v", job)
			}
		})
	}
}

func TestSubmitClaudeEncodesLines(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	id := uuid.NewString()
	keys := []string{"skills/acme/one@sha:" + uuid.NewString(), "two " + uuid.NewString()}
	job, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{
		claudeReq(keys[0], "first"), claudeReq(keys[1], "second"),
	})
	if err != nil {
		t.Fatalf("SubmitClaude: %v", err)
	}
	if job.Provider != "anthropic" || job.Model != "claude-opus-5-5@default" {
		t.Errorf("job provider/model: got = %q/%q", job.Provider, job.Model)
	}
	data, ok := env.gcs.get(testPrefix + "/" + id + "/input.jsonl")
	if !ok {
		t.Fatal("input.jsonl not written")
	}
	lines := jsonLines(t, data)
	if len(lines) != 2 {
		t.Fatalf("lines: got = %d, want = 2", len(lines))
	}
	for i, line := range lines {
		if got, want := line["custom_id"], ref(keys[i]); got != want {
			t.Errorf("line %d custom_id: got = %v, want = %q", i, got, want)
		}
		req, _ := line["request"].(map[string]any)
		if _, ok := req["model"]; ok {
			t.Errorf("line %d: request carries model", i)
		}
		if _, ok := req["stream"]; ok {
			t.Errorf("line %d: request carries stream", i)
		}
		if got := req["anthropic_version"]; got != "vertex-2023-10-16" {
			t.Errorf("line %d anthropic_version: got = %v", i, got)
		}
		if got := req["max_tokens"]; got != float64(1024) {
			t.Errorf("line %d max_tokens: got = %v", i, got)
		}
		if msgs, _ := req["messages"].([]any); len(msgs) != 1 {
			t.Errorf("line %d messages: got = %v", i, req["messages"])
		}
	}
}

func TestSubmitGeminiEncodesLines(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	id := uuid.NewString()
	key := uuid.NewString()
	if _, err := env.client.SubmitGemini(t.Context(), id, "gemini-3.8-flash", []GeminiRequest{geminiReq(key, "review this")}); err != nil {
		t.Fatalf("SubmitGemini: %v", err)
	}
	data, _ := env.gcs.get(testPrefix + "/" + id + "/input.jsonl")
	lines := jsonLines(t, data)
	if len(lines) != 1 {
		t.Fatalf("lines: got = %d, want = 1", len(lines))
	}
	want := map[string]any{
		"request": map[string]any{
			"contents":          []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "review this"}}}},
			"systemInstruction": map[string]any{"role": "user", "parts": []any{map[string]any{"text": "You review skills."}}},
			"generationConfig":  map[string]any{"temperature": 0.2, "maxOutputTokens": float64(2048)},
			"labels":            map[string]any{"team": "skills", keyLabel: ref(key)},
		},
	}
	if diff := cmp.Diff(want, lines[0], cmp.Comparer(func(a, b float64) bool { return float32(a) == float32(b) })); diff != "" {
		t.Errorf("line (-want, +got):\n%s", diff)
	}
}

func TestSubmitIdempotent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup runs before the submission under test.
		setup func(t *testing.T, env *testEnv, id string)
	}{{
		name: "id already submitted",
		setup: func(t *testing.T, env *testEnv, id string) {
			if _, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")}); err != nil {
				t.Fatalf("first submit: %v", err)
			}
		},
	}, {
		name: "create response lost",
		setup: func(_ *testing.T, env *testEnv, _ string) {
			env.vertex.dropCreateResponses = 1
		},
	}, {
		name: "list filter ignored, same name from another prefix",
		setup: func(_ *testing.T, env *testEnv, id string) {
			env.vertex.ignoreFilter = true
			env.vertex.jobs = append(env.vertex.jobs, &vertexJob{
				Name:        "projects/" + testProject + "/locations/" + testLocation + "/batchPredictionJobs/1",
				DisplayName: id,
				State:       string(genai.JobStateSucceeded),
				CreateTime:  "2027-01-01T00:00:00Z",
				InputConfig: map[string]any{"instancesFormat": "jsonl", "gcsSource": map[string]any{"uris": []string{"gs://other/" + id + "/input.jsonl"}}},
			})
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			id := uuid.NewString()
			tt.setup(t, env, id)
			job, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
			if err != nil {
				t.Fatalf("SubmitClaude: %v", err)
			}
			if got := env.vertex.createCount(); got != 1 {
				t.Errorf("creates: got = %d, want = 1", got)
			}
			if strings.HasSuffix(job.Name, "/batchPredictionJobs/1") {
				t.Errorf("job: got = %q, the other prefix's job", job.Name)
			}
			found, ok, err := env.client.Find(t.Context(), id)
			if err != nil || !ok {
				t.Fatalf("Find: ok = %v, err = %v", ok, err)
			}
			if diff := cmp.Diff(job, found); diff != "" {
				t.Errorf("Find disagrees with Submit (-submit, +find):\n%s", diff)
			}
		})
	}
}

func TestSubmitRejectsInvalid(t *testing.T) {
	t.Parallel()
	reserved := geminiReq("k", "p")
	reserved.Config.Labels = map[string]string{keyLabel: "x"}
	noTokens := claudeReq("k", "p")
	noTokens.Params.MaxTokens = 0
	tests := []struct {
		name   string
		submit func(*Client) error
	}{{
		name: "id with a slash",
		submit: func(c *Client) error {
			_, err := c.SubmitClaude(t.Context(), "a/b", "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
			return err
		},
	}, {
		name: "prefixed model",
		submit: func(c *Client) error {
			_, err := c.SubmitClaude(t.Context(), "id", "anthropic/claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
			return err
		},
	}, {
		name: "no requests",
		submit: func(c *Client) error {
			_, err := c.SubmitGemini(t.Context(), "id", "gemini-3.8-flash", nil)
			return err
		},
	}, {
		name: "duplicate key",
		submit: func(c *Client) error {
			_, err := c.SubmitClaude(t.Context(), "id", "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p"), claudeReq("k", "q")})
			return err
		},
	}, {
		name: "empty key",
		submit: func(c *Client) error {
			_, err := c.SubmitGemini(t.Context(), "id", "gemini-3.8-flash", []GeminiRequest{geminiReq("", "p")})
			return err
		},
	}, {
		name: "missing max tokens",
		submit: func(c *Client) error {
			_, err := c.SubmitClaude(t.Context(), "id", "claude-opus-5-5@default", []ClaudeRequest{noTokens})
			return err
		},
	}, {
		name: "reserved label",
		submit: func(c *Client) error {
			_, err := c.SubmitGemini(t.Context(), "id", "gemini-3.8-flash", []GeminiRequest{reserved})
			return err
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			if err := tt.submit(env.client); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err: got = %v, want = %v", err, ErrInvalidRequest)
			}
			if n := env.vertex.createCount(); n != 0 {
				t.Errorf("creates: got = %d, want = 0", n)
			}
			if len(env.gcs.objects) != 0 {
				t.Errorf("objects written: got = %d, want = 0", len(env.gcs.objects))
			}
		})
	}
}

func TestSubmitRejectsReusedIDForOtherModel(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	id := uuid.NewString()
	if _, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")}); err != nil {
		t.Fatalf("SubmitClaude: %v", err)
	}
	_, err := env.client.SubmitGemini(t.Context(), id, "gemini-3.8-flash", []GeminiRequest{geminiReq("k", "p")})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err: got = %v, want = %v", err, ErrInvalidRequest)
	}
	if n := env.vertex.createCount(); n != 1 {
		t.Errorf("creates: got = %d, want = 1", n)
	}
}

// TestSubmitRejectsDestinationCollision covers a job someone else created
// with this submission's display name and input but its own destination:
// adopting it, or creating a second job beside it, would hide that the staged
// input was handed to a job this client did not create.
func TestSubmitRejectsDestinationCollision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// dest is the colliding job's output prefix; empty omits outputConfig.
		dest string
		// raced adds the job between Submit's first lookup and its create.
		raced bool
	}{{
		name: "foreign bucket",
		dest: "gs://other-bucket/UNAVAILABLE/",
	}, {
		name:  "foreign bucket, raced after staging",
		dest:  "gs://other-bucket/UNAVAILABLE/",
		raced: true,
	}, {
		name: "another id's directory in this bucket",
		dest: "gs://" + testBucket + "/" + testPrefix + "/other-id/output/",
	}, {
		name: "no output config",
		dest: "",
	}, {
		name:  "no output config, raced after staging",
		dest:  "",
		raced: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			id := uuid.NewString()
			foreign := &vertexJob{
				Name:        "projects/" + testProject + "/locations/" + testLocation + "/batchPredictionJobs/666",
				DisplayName: id,
				State:       string(genai.JobStatePending),
				CreateTime:  "2026-10-18T00:00:00Z",
				InputConfig: map[string]any{"instancesFormat": "jsonl", "gcsSource": map[string]any{
					"uris": []string{"gs://" + testBucket + "/" + testPrefix + "/" + id + "/input.jsonl"},
				}},
			}
			if tt.dest != "" {
				foreign.OutputConfig = &vertexOutputConfig{PredictionsFormat: "jsonl"}
				foreign.OutputConfig.GCSDestination.OutputURIPrefix = tt.dest
			}
			if tt.raced {
				env.vertex.raceJob, env.vertex.raceAfterLists = foreign, 1
			} else {
				env.vertex.jobs = append(env.vertex.jobs, foreign)
			}

			_, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("SubmitClaude: got = %v, want = %v", err, ErrInvalidRequest)
			}
			if !strings.Contains(err.Error(), foreign.Name) {
				t.Errorf("SubmitClaude: got = %v, want the colliding job named", err)
			}
			if IsRetryable(err) {
				t.Errorf("IsRetryable(%v): got = true, want = false", err)
			}
			if n := env.vertex.createCount(); n != 0 {
				t.Errorf("creates: got = %d, want = 0", n)
			}
			if _, ok, err := env.client.Find(t.Context(), id); ok || !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("Find: ok = %v, err = %v, want = %v", ok, err, ErrInvalidRequest)
			}
		})
	}
}

// TestFindNormalizesDestinationSlash adopts a job whose reported output
// prefix differs from the submitted one only by its trailing slash.
func TestFindNormalizesDestinationSlash(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	id := uuid.NewString()
	job, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq("k", "p")})
	if err != nil {
		t.Fatalf("SubmitClaude: %v", err)
	}
	env.vertex.mu.Lock()
	j := env.vertex.job("/v1beta1/" + job.Name)
	j.OutputConfig.GCSDestination.OutputURIPrefix = strings.TrimSuffix(j.OutputConfig.GCSDestination.OutputURIPrefix, "/")
	env.vertex.mu.Unlock()

	found, ok, err := env.client.Find(t.Context(), id)
	if err != nil || !ok {
		t.Fatalf("Find: ok = %v, err = %v", ok, err)
	}
	if diff := cmp.Diff(job, found); diff != "" {
		t.Errorf("Find (-submit, +find):\n%s", diff)
	}
}

// TestSubmitConcurrentConverges races two Submits of one id past their final
// lookup, so both create a job: they must settle on the earlier one and
// cancel the other before it runs.
func TestSubmitConcurrentConverges(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	id := uuid.NewString()
	reqs := []ClaudeRequest{claudeReq(uuid.NewString(), "p")}
	// Each caller lists once in Find and once before Create.
	env.vertex.pairedLists = 4
	var (
		wg   sync.WaitGroup
		jobs [2]Job
		errs [2]error
	)
	for i := range 2 {
		wg.Go(func() {
			jobs[i], errs[i] = env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", reqs)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := env.vertex.createCount(); got != 2 {
		t.Fatalf("creates: got = %d, want = 2 (the race did not happen)", got)
	}
	if jobs[0].Name != jobs[1].Name {
		t.Errorf("callers disagree: got = %q and %q", jobs[0].Name, jobs[1].Name)
	}
	var live []string
	for name, state := range env.vertex.states() {
		switch genai.JobState(state) {
		case genai.JobStateCancelling, genai.JobStateCancelled:
		default:
			live = append(live, name)
		}
	}
	if len(live) != 1 || live[0] != jobs[0].Name {
		t.Errorf("live jobs: got = %v, want only %q", live, jobs[0].Name)
	}
	found, ok, err := env.client.Find(t.Context(), id)
	if err != nil || !ok || found.Name != jobs[0].Name {
		t.Errorf("Find: got = %q, %v, %v, want = %q", found.Name, ok, err, jobs[0].Name)
	}
}

// TestSubmitRejectsDifferentRowsForStagedID stages an id whose job was never
// created, then submits other rows under it: the staged objects must not be
// overwritten, or a concurrent first caller would create its job over them.
func TestSubmitRejectsDifferentRowsForStagedID(t *testing.T) {
	t.Parallel()
	key := uuid.NewString()
	tests := []struct {
		name   string
		submit func(c *Client, id string) error
	}{{
		name: "different prompt",
		submit: func(c *Client, id string) error {
			_, err := c.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq(key, "other")})
			return err
		},
	}, {
		name: "different key",
		submit: func(c *Client, id string) error {
			_, err := c.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq(uuid.NewString(), "p")})
			return err
		},
	}, {
		name: "extra row",
		submit: func(c *Client, id string) error {
			_, err := c.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq(key, "p"), claudeReq(uuid.NewString(), "q")})
			return err
		},
	}, {
		name: "different model",
		submit: func(c *Client, id string) error {
			_, err := c.SubmitClaude(t.Context(), id, "claude-sonnet-5-5@default", []ClaudeRequest{claudeReq(key, "p")})
			return err
		},
	}, {
		name: "different provider",
		submit: func(c *Client, id string) error {
			_, err := c.SubmitGemini(t.Context(), id, "claude-opus-5-5@default", []GeminiRequest{geminiReq(key, "p")})
			return err
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			id := uuid.NewString()
			env.vertex.rejectCreates = 1
			if _, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", []ClaudeRequest{claudeReq(key, "p")}); err == nil {
				t.Fatal("first submit: got = nil, want the scripted rejection")
			}
			input, _ := env.gcs.get(testPrefix + "/" + id + "/input.jsonl")
			man, _ := env.gcs.get(testPrefix + "/" + id + "/manifest.json")

			err := tt.submit(env.client, id)
			if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), id) {
				t.Errorf("second submit: got = %v, want %v naming %q", err, ErrInvalidRequest, id)
			}
			if got, _ := env.gcs.get(testPrefix + "/" + id + "/input.jsonl"); !bytes.Equal(got, input) {
				t.Errorf("input changed:\n got = %s\nwant = %s", got, input)
			}
			if got, _ := env.gcs.get(testPrefix + "/" + id + "/manifest.json"); !bytes.Equal(got, man) {
				t.Errorf("manifest changed:\n got = %s\nwant = %s", got, man)
			}
			if n := env.vertex.createCount(); n != 1 {
				t.Errorf("creates: got = %d, want = 1", n)
			}
		})
	}
}

// TestSubmitResumesStagedID re-submits the same rows after a crash that
// left staged objects but no job.
func TestSubmitResumesStagedID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// dropInput removes the input, as a crash between the two writes.
		dropInput bool
	}{{
		name:      "manifest written, input not",
		dropInput: true,
	}, {
		name: "manifest and input written",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			id := uuid.NewString()
			reqs := []ClaudeRequest{claudeReq(uuid.NewString(), "p")}
			env.vertex.rejectCreates = 1
			if _, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", reqs); err == nil {
				t.Fatal("first submit: got = nil, want the scripted rejection")
			}
			inputObject := testPrefix + "/" + id + "/input.jsonl"
			input, _ := env.gcs.get(inputObject)
			if tt.dropInput {
				env.gcs.mu.Lock()
				delete(env.gcs.objects, inputObject)
				env.gcs.mu.Unlock()
			}

			job, err := env.client.SubmitClaude(t.Context(), id, "claude-opus-5-5@default", reqs)
			if err != nil {
				t.Fatalf("re-submit: %v", err)
			}
			if states := env.vertex.states(); len(states) != 1 || states[job.Name] == "" {
				t.Errorf("jobs: got = %v, want only %q", states, job.Name)
			}
			if got, _ := env.gcs.get(inputObject); !bytes.Equal(got, input) {
				t.Errorf("input:\n got = %s\nwant = %s", got, input)
			}
		})
	}
}
