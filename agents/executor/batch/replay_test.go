/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

// The replay tests run output recorded from real Vertex batch jobs through
// the client and the fakes, so the fakes' line shapes are checked against
// what Vertex writes. testdata/replay/README.md describes the layout and how
// to record it.

const replayRoot = "testdata/replay"

// replayCapture is capture.json: how the recording was made.
type replayCapture struct {
	Model        string `json:"model"`
	Location     string `json:"location"`
	Date         string `json:"date"`
	GenaiVersion string `json:"genai_version"`
	// State is the job's terminal state as Vertex reported it.
	State genai.JobState `json:"state"`
	// Unobtained lists case keys the recording could not elicit, each
	// skipped rather than failed.
	Unobtained []string `json:"unobtained,omitempty"`
}

// replayCase is a request key in requests.json and what its result must be.
type replayCase struct {
	key   string
	check func(t *testing.T, res Result)
}

// The keys every recording carries. "missing" is a plain request whose
// output line the replay drops; "dropped_label" has its echoed labels
// removed, so it is matched by request digest.
const (
	replayMissing      = "missing"
	replayDroppedLabel = "dropped_label"
)

func wantOK(stop string) func(*testing.T, Result) {
	return func(t *testing.T, res Result) {
		if res.Err != nil || res.Text == "" || res.Stop != stop || res.Usage.In <= 0 || res.Usage.Out <= 0 {
			t.Errorf("got = %+v, want text, stop %q, and usage with no error", res, stop)
		}
	}
}

func wantTruncated(t *testing.T, res Result) {
	if !errors.Is(res.Err, ErrTruncated) || res.Text == "" {
		t.Errorf("got = %+v, want %v with partial text", res, ErrTruncated)
	}
}

func wantRefused(category string) func(*testing.T, Result) {
	return func(t *testing.T, res Result) {
		if !errors.Is(res.Err, ErrRefused) || res.Refusal == nil || res.Text != "" {
			t.Fatalf("got = %+v, want %v with a refusal and no text", res, ErrRefused)
		}
		if category != "" && res.Refusal.Category != category {
			t.Errorf("refusal category: got = %q, want = %q", res.Refusal.Category, category)
		}
	}
}

func wantLineError(retryable bool) func(*testing.T, Result) {
	return func(t *testing.T, res Result) {
		if _, ok := errors.AsType[*lineError](res.Err); !ok {
			t.Fatalf("got = %+v, want a per-line error", res)
		}
		if got := IsRetryable(res.Err); got != retryable {
			t.Errorf("IsRetryable(%v): got = %v, want = %v", res.Err, got, retryable)
		}
	}
}

func wantMissing(t *testing.T, res Result) {
	if !errors.Is(res.Err, ErrMissing) {
		t.Errorf("got = %+v, want %v", res, ErrMissing)
	}
}

func TestReplayClaude(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(replayRoot, "claude")
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no recorded claude batch output; see testdata/replay/README.md")
	}
	replay(t, dir, providerAnthropic, []replayCase{
		{key: "success", check: wantOK(string(anthropic.StopReasonEndTurn))},
		{key: "refusal", check: wantRefused("")},
		{key: "max_tokens", check: wantTruncated},
		// An invalid request, which no retry fixes.
		{key: "errored", check: wantLineError(false)},
		{key: replayMissing, check: wantMissing},
	})
}

func TestReplayGemini(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(replayRoot, "gemini")
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no recorded gemini batch output; see testdata/replay/README.md")
	}
	replay(t, dir, providerGoogle, []replayCase{
		{key: "stop", check: wantOK(string(genai.FinishReasonStop))},
		{key: "max_tokens", check: wantTruncated},
		{key: "safety", check: wantRefused(string(genai.FinishReasonSafety))},
		{key: "prompt_blocked", check: wantRefused("")},
		{key: replayDroppedLabel, check: wantOK(string(genai.FinishReasonStop))},
		{key: "no_candidates", check: wantLineError(true)},
		{key: replayMissing, check: wantMissing},
	})
}

// replay submits the recorded requests to the fakes, checks that they encode
// to the recorded input and manifest, then serves the recorded output as the
// job's and checks each case's result.
func replay(t *testing.T, dir, provider string, cases []replayCase) {
	t.Helper()
	var capture replayCapture
	readJSONFile(t, filepath.Join(dir, "capture.json"), &capture)
	env := newTestEnv(t)
	const id = "replay"

	var (
		job Job
		err error
	)
	switch provider {
	case providerAnthropic:
		var reqs []struct {
			Key    string                     `json:"key"`
			Params anthropic.MessageNewParams `json:"params"`
		}
		readJSONFile(t, filepath.Join(dir, "requests.json"), &reqs)
		claude := make([]ClaudeRequest, 0, len(reqs))
		for _, r := range reqs {
			claude = append(claude, ClaudeRequest{Key: r.Key, Params: r.Params})
		}
		job, err = env.client.SubmitClaude(t.Context(), id, capture.Model, claude)
	default:
		var reqs []struct {
			Key      string                       `json:"key"`
			Contents []*genai.Content             `json:"contents"`
			Config   *genai.GenerateContentConfig `json:"config"`
		}
		readJSONFile(t, filepath.Join(dir, "requests.json"), &reqs)
		gemini := make([]GeminiRequest, 0, len(reqs))
		for _, r := range reqs {
			gemini = append(gemini, GeminiRequest{Key: r.Key, Contents: r.Contents, Config: r.Config})
		}
		job, err = env.client.SubmitGemini(t.Context(), id, capture.Model, gemini)
	}
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	wantInput, err := os.ReadFile(filepath.Join(dir, "input.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	gotInput, _ := env.gcs.get(testPrefix + "/" + id + "/" + inputObject)
	if diff := cmp.Diff(string(wantInput), string(gotInput)); diff != "" {
		t.Errorf("re-encoded input differs from the recorded input (-recorded, +encoded):\n%s", diff)
	}
	var wantManifest manifest
	readJSONFile(t, filepath.Join(dir, "manifest.json"), &wantManifest)
	wantManifestRaw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	gotManifestRaw, _ := env.gcs.get(testPrefix + "/" + id + "/" + manifestObject)
	if diff := cmp.Diff(string(wantManifestRaw), string(gotManifestRaw)); diff != "" {
		t.Errorf("re-encoded manifest differs from the recorded manifest (-recorded, +encoded):\n%s", diff)
	}

	env.vertex.finish(t, env.gcs, job.Name, capture.State)
	outDir := strings.TrimPrefix(env.vertex.outputDir(job.Name), "gs://"+testBucket+"/")
	outputs, err := filepath.Glob(filepath.Join(dir, "output", "*.jsonl"))
	if err != nil || len(outputs) == 0 {
		t.Fatalf("no recorded output under %s: %v", dir, err)
	}
	for _, f := range outputs {
		env.gcs.put(path.Join(outDir, filepath.Base(f)), replayOutput(t, f, wantManifest))
	}

	got := collect(t, env.client, job)
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			if slices.Contains(capture.Unobtained, tc.key) {
				t.Skipf("the %s recording could not elicit %q", capture.Date, tc.key)
			}
			res, ok := got[tc.key]
			if !ok {
				t.Fatalf("no result for %q; requests.json must carry every case key", tc.key)
			}
			tc.check(t, res)
		})
	}
}

// replayOutput returns a recorded output file with the "missing" key's line
// dropped and the "dropped_label" key's echoed labels removed.
func replayOutput(t *testing.T, file string, m manifest) []byte {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for raw := range bytes.Lines(data) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var line outputLine
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		// A fresh collector per line, so identical requests do not consume
		// each other's digest match.
		key, _ := newCollector(m, nil).key(line)
		switch key {
		case replayMissing:
			continue
		case replayDroppedLabel:
			var fields, req map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(fields["request"], &req); err != nil {
				t.Fatal(err)
			}
			delete(req, "labels")
			if fields["request"], err = json.Marshal(req); err != nil {
				t.Fatal(err)
			}
			if raw, err = json.Marshal(fields); err != nil {
				t.Fatal(err)
			}
		}
		out.Write(raw)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func readJSONFile(t *testing.T, file string, v any) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// scrubRules match identifiers a recording can leak. Each captured value must
// be one of the placeholders the README names; a rule with no placeholders
// rejects every match.
var scrubRules = []struct {
	name    string
	pattern *regexp.Regexp
	allowed []string
	// isolated rejects a match that touches a letter or digit, so digit runs
	// inside hex digests and longer numbers are skipped.
	isolated bool
}{
	{name: "project", pattern: regexp.MustCompile(`projects/([\w.-]+)`), allowed: []string{"example-project", "0"}},
	{name: "project field", pattern: regexp.MustCompile(`"project(?:_?[iI]d|_?[nN]umber)?"\s*:\s*"?([^",}\s]*)"?`), allowed: []string{"example-project", "0"}},
	{name: "bucket", pattern: regexp.MustCompile(`gs://([\w.-]+)`), allowed: []string{"example-bucket"}},
	{name: "bucket URL", pattern: regexp.MustCompile(`storage\.googleapis\.com/([\w.-]+)`), allowed: []string{"example-bucket"}},
	{name: "bucket field", pattern: regexp.MustCompile(`"bucket"\s*:\s*"([^"]*)"`), allowed: []string{"example-bucket"}},
	{name: "job", pattern: regexp.MustCompile(`batchPredictionJobs/([\w.-]+)`), allowed: []string{"0"}},
	// Bounds are checked outside the pattern so a separator is not consumed
	// and adjacent numbers are both found; \b would miss "_123456789012".
	{name: "project number", pattern: regexp.MustCompile(`(\d{12})`), isolated: true},
	{name: "email", pattern: regexp.MustCompile(`([\w.%+-]+@[\w-]+(?:\.[\w-]+)+)`)},
}

// scrubEscape matches the JSON and URL escapes an identifier can hide behind.
var scrubEscape = regexp.MustCompile(`\\/|\\u00[0-9a-fA-F]{2}|%[0-9a-fA-F]{2}`)

// scrubNormalize undoes one layer of scrubEscape's escapes, so the rules see
// "/" and "@".
func scrubNormalize(data string) string {
	return scrubEscape.ReplaceAllStringFunc(data, func(e string) string {
		if e == `\/` {
			return "/"
		}
		b, _ := strconv.ParseUint(e[len(e)-2:], 16, 8)
		return string(rune(b))
	})
}

// scrubAlnum reports whether the byte at i is a letter or digit.
func scrubAlnum(d string, i int) bool {
	if i < 0 || i >= len(d) {
		return false
	}
	c := d[i]
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// scrubFindings returns each identifier in data that is not a placeholder,
// checking data and every layer of unescaping until none is left; each layer
// shortens the text, so the loop ends.
func scrubFindings(data string) []string {
	var out []string
	for d, prev := data, ""; d != prev; d, prev = scrubNormalize(d), d {
		for _, r := range scrubRules {
			for _, m := range r.pattern.FindAllStringSubmatchIndex(d, -1) {
				if r.isolated && (scrubAlnum(d, m[0]-1) || scrubAlnum(d, m[1])) {
					continue
				}
				v := d[m[2]:m[3]]
				if f := r.name + ": " + v; !slices.Contains(r.allowed, v) && !slices.Contains(out, f) {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

func TestScrubFindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want []string
	}{{
		name: "placeholders",
		data: `{"name":"projects/0/locations/us/batchPredictionJobs/0","uri":"gs://example-bucket/x","project":"example-project"}`,
	}, {
		name: "real project and job",
		data: `"projects/prod-skills-a1b2/locations/us/batchPredictionJobs/5528394"`,
		want: []string{"project: prod-skills-a1b2", "job: 5528394"},
	}, {
		name: "real bucket",
		data: `gs://acme-batch-staging/input.jsonl https://storage.googleapis.com/acme-batch-staging/o`,
		want: []string{"bucket: acme-batch-staging", "bucket URL: acme-batch-staging"},
	}, {
		name: "project number and field",
		data: `{"projectNumber":"123456789012","project_id":"acme-prod"}`,
		want: []string{"project field: 123456789012", "project field: acme-prod", "project number: 123456789012"},
	}, {
		name: "service account",
		data: `"principal":"svc@acme-prod.iam.gserviceaccount.com"`,
		want: []string{"email: svc@acme-prod.iam.gserviceaccount.com"},
	}, {
		name: "JSON-escaped slashes",
		data: `{"name":"projects\/acme-prod\/locations\/us\/batchPredictionJobs\/5528394","uri":"gs:\/\/acme-batch-staging\/input.jsonl"}`,
		want: []string{"project: acme-prod", "bucket: acme-batch-staging", "job: 5528394"},
	}, {
		name: "unicode-escaped at",
		data: `{"principal":"svc\u0040acme-prod.iam.gserviceaccount.com"}`,
		want: []string{"email: svc@acme-prod.iam.gserviceaccount.com"},
	}, {
		name: "percent-escaped at",
		data: `{"url":"https://x/?sa=svc%40acme-prod.iam.gserviceaccount.com"}`,
		want: []string{"email: svc@acme-prod.iam.gserviceaccount.com"},
	}, {
		name: "unquoted project number field",
		data: `{"projectNumber":1234567890}`,
		want: []string{"project field: 1234567890"},
	}, {
		name: "unquoted placeholder project number",
		data: `{"projectNumber":0}`,
	}, {
		name: "project number after an underscore",
		data: `{"x":"prediction_123456789012"}`,
		want: []string{"project number: 123456789012"},
	}, {
		name: "digits inside a hex digest",
		data: `{"input_sha256":"ab123456789012cd","ref":"0f123456789012e9"}`,
	}, {
		name: "longer digit runs are not project numbers",
		data: `{"t":"1234567890123","n":12345678901}`,
	}, {
		name: "adjacent project numbers",
		data: `{"ids":[123456789012,210987654321],"x":"111111111111 222222222222"}`,
		want: []string{"project number: 123456789012", "project number: 210987654321", "project number: 111111111111", "project number: 222222222222"},
	}, {
		name: "double percent-escaped at",
		data: `{"u":"https://x/?sa=svc%2540acme-prod.iam.gserviceaccount.com"}`,
		want: []string{"email: svc@acme-prod.iam.gserviceaccount.com"},
	}, {
		name: "double unicode-escaped slashes",
		data: `{"u":"gs:\\u002f\\u002facme-bucket"}`,
		want: []string{"bucket: acme-bucket"},
	}, {
		name: "double JSON-escaped slash",
		data: `{"m":"projects\\\/acme-prod"}`,
		want: []string{"project: acme-prod"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, scrubFindings(tt.data)); diff != "" {
				t.Errorf("scrubFindings (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestReplayFixturesScrubbed keeps recordings from publishing real project,
// bucket, or job identifiers: this testdata ships in a public module.
func TestReplayFixturesScrubbed(t *testing.T) {
	t.Parallel()
	err := filepath.WalkDir(replayRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, f := range scrubFindings(string(data)) {
			t.Errorf("%s: %s is not a placeholder; scrub it as testdata/replay/README.md describes", p, f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
