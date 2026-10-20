/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/genai"

	"chainguard.dev/driftlessaf/agents/executor/retry"
)

// The fakes below serve the Vertex and Cloud Storage JSON APIs over HTTP, so
// the tests drive the real genai and storage clients, including their
// request and response conversion, rather than a stand-in for them.

const (
	testProject  = "batch-test-project"
	testLocation = "us"
	testBucket   = "batch-test-bucket"
	testPrefix   = "batch/jobs"
)

// fakeGCS is an in-memory Cloud Storage JSON API: multipart upload, media
// download, and prefix listing.
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string][]byte
	// failReads makes the next n media downloads fail with 503.
	failReads int
	// breakAt, when positive, makes the next output download stop after
	// that many bytes, and failResume then fails the client's ranged resume
	// with 503: the read errors with some lines already consumed.
	breakAt    int
	failResume bool
	// loseWrite names an object whose next upload is stored but answered
	// 503, as when the response is lost.
	loseWrite string
}

func (f *fakeGCS) put(name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[name] = data
}

func (f *fakeGCS) get(name string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[name]
	return data, ok
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/"+testBucket+"/o"):
		f.upload(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/"+testBucket+"/o":
		f.list(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/"+testBucket+"/o/") && r.URL.Query().Get("alt") == "media":
		f.download(w, r)
	default:
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, `{"error":{"code":501,"message":"fake GCS: unsupported %s %s"}}`, r.Method, r.URL.Path)
	}
}

func (f *fakeGCS) upload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || name == "" {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	if _, err := mr.NextPart(); err != nil { // metadata
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(part)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// ifGenerationMatch=0 is the create-only precondition; the fake keeps no
	// generations, so any other value is unsupported rather than ignored.
	if match, ok := r.URL.Query()["ifGenerationMatch"]; ok {
		if match[0] != "0" {
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprintf(w, `{"error":{"code":501,"message":"fake GCS: unsupported ifGenerationMatch=%s"}}`, match[0])
			return
		}
		if _, exists := f.objects[name]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			fmt.Fprint(w, `{"error":{"code":412,"message":"At least one of the pre-conditions you specified did not hold.","errors":[{"reason":"conditionNotMet"}]}}`)
			return
		}
	}
	f.objects[name] = data
	if name == f.loseWrite {
		f.loseWrite = ""
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":503,"message":"scripted lost response"}}`)
		return
	}
	fmt.Fprintf(w, `{"name":%q,"bucket":%q,"size":"%d"}`, name, testBucket, len(data))
}

func (f *fakeGCS) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	f.mu.Lock()
	var items []map[string]string
	for name, data := range f.objects {
		if strings.HasPrefix(name, prefix) {
			items = append(items, map[string]string{"name": name, "bucket": testBucket, "size": strconv.Itoa(len(data))})
		}
	}
	f.mu.Unlock()
	slices.SortFunc(items, func(a, b map[string]string) int { return strings.Compare(a["name"], b["name"]) })
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "storage#objects", "items": items})
}

func (f *fakeGCS) download(w http.ResponseWriter, r *http.Request) {
	name, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/storage/v1/b/"+testBucket+"/o/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	resumed := r.Header.Get("Range") != ""
	fail := f.failReads > 0 || (resumed && f.failResume)
	if fail {
		if resumed && f.failResume {
			f.failResume = false
		} else {
			f.failReads--
		}
	}
	data, ok := f.objects[name]
	broken := ok && !fail && !resumed && f.breakAt > 0 && strings.Contains(name, "/"+outputDir)
	at := f.breakAt
	if broken {
		f.breakAt, f.failResume = 0, true
	}
	f.mu.Unlock()
	switch {
	case fail:
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":503,"message":"scripted unavailable"}}`)
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"code":404,"message":"No such object: %s"}}`, name)
	case resumed:
		// The client resumes a broken read from the bytes it has seen.
		start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-"))
		if err != nil || start > len(data) {
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprintf(w, `{"error":{"code":501,"message":"fake GCS: unsupported Range %q"}}`, r.Header.Get("Range"))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start:])
	case broken:
		// Declaring the full length and sending less makes the client's
		// read fail with io.ErrUnexpectedEOF.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data[:at])
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}
}

// vertexJob is a BatchPredictionJob in the Vertex REST shape.
type vertexJob struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Model       string `json:"model"`
	State       string `json:"state"`
	CreateTime  string `json:"createTime"`
	InputConfig any    `json:"inputConfig"`
	// OutputConfig is nil for a job listed without one.
	OutputConfig    *vertexOutputConfig `json:"outputConfig,omitempty"`
	OutputInfo      map[string]string   `json:"outputInfo,omitempty"`
	CompletionStats map[string]string   `json:"completionStats,omitempty"`
}

type vertexOutputConfig struct {
	PredictionsFormat string `json:"predictionsFormat"`
	GCSDestination    struct {
		OutputURIPrefix string `json:"outputUriPrefix"`
	} `json:"gcsDestination"`
}

// fakeVertex is an in-memory Vertex batchPredictionJobs API.
type fakeVertex struct {
	mu      sync.Mutex
	jobs    []*vertexJob
	creates []map[string]any
	filters []string
	// dropCreateResponses makes the next n creates succeed but answer 503,
	// as when the response is lost after Vertex accepted the job.
	dropCreateResponses int
	// ignoreFilter lists every job whatever the filter says.
	ignoreFilter bool
	// raceJob joins the job list once raceAfterLists lists have been
	// answered, as a job someone else created between two of the client's
	// lookups.
	raceJob        *vertexJob
	raceAfterLists int
	// rejectCreates makes the next n creates fail with 400 and create
	// nothing, leaving the staged input behind as a crash would.
	rejectCreates int
	// pairedLists holds that many List responses in pairs: each takes its
	// snapshot, then waits for a second, so neither caller sees the other act.
	pairedLists int
	waiting     chan struct{}
	// unlisted names a job List omits while Get still serves it, as when the
	// list lags behind a create.
	unlisted string
	// holdCreate holds the next create after its CreateTime is stamped and
	// before the job is listed.
	holdCreate *createHold
	// createdModel rewrites the model a created job reports, as when Vertex
	// resolves an alias to a dated version.
	createdModel func(string) string
}

// createHold pauses one create: arrived closes once its CreateTime is
// stamped, and the job is committed when release closes.
type createHold struct {
	arrived, release chan struct{}
}

const jobsPath = "/v1beta1/projects/" + testProject + "/locations/" + testLocation + "/batchPredictionJobs"

func (f *fakeVertex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == jobsPath {
		var buf bytes.Buffer
		f.mu.Lock()
		f.list(&buf, r)
		f.mu.Unlock()
		f.pair(r)
		_, _ = w.Write(buf.Bytes())
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == jobsPath {
		f.create(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":cancel"):
		if j := f.job(strings.TrimSuffix(r.URL.Path, ":cancel")); j != nil {
			j.State = string(genai.JobStateCancelling)
			fmt.Fprint(w, `{}`)
			return
		}
		f.notFound(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, jobsPath+"/"):
		if j := f.job(r.URL.Path); j != nil {
			_ = json.NewEncoder(w).Encode(j)
			return
		}
		f.notFound(w, r)
	default:
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, `{"error":{"code":501,"message":"fake Vertex: unsupported %s %s"}}`, r.Method, r.URL.Path)
	}
}

// pair holds a List response until another List has taken its snapshot,
// while pairedLists lasts.
func (f *fakeVertex) pair(r *http.Request) {
	f.mu.Lock()
	if f.pairedLists == 0 {
		f.mu.Unlock()
		return
	}
	f.pairedLists--
	if partner := f.waiting; partner != nil {
		f.waiting = nil
		f.mu.Unlock()
		close(partner)
		return
	}
	ch := make(chan struct{})
	f.waiting = ch
	f.mu.Unlock()
	select {
	case <-ch:
	case <-r.Context().Done():
	}
}

func (f *fakeVertex) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `{"error":{"code":404,"message":"not found: %s","status":"NOT_FOUND"}}`, r.URL.Path)
}

// job finds a job by its request path, which is the resource name with the
// API version in front.
func (f *fakeVertex) job(path string) *vertexJob {
	name := strings.TrimPrefix(path, "/v1beta1/")
	for _, j := range f.jobs {
		if j.Name == name {
			return j
		}
	}
	return nil
}

func (f *fakeVertex) create(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.creates = append(f.creates, body)
	// Vertex stamps CreateTime on receipt, which can precede the job being
	// listed.
	created := time.Date(2026, 10, 18, 0, 0, len(f.creates), 0, time.UTC)
	hold := f.holdCreate
	f.holdCreate = nil
	f.mu.Unlock()
	if hold != nil {
		close(hold.arrived)
		select {
		case <-hold.release:
		case <-r.Context().Done():
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectCreates > 0 {
		f.rejectCreates--
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":400,"message":"scripted rejection","status":"INVALID_ARGUMENT"}}`)
		return
	}
	raw, _ := json.Marshal(body)
	j := &vertexJob{}
	if err := json.Unmarshal(raw, j); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if f.createdModel != nil {
		j.Model = f.createdModel(j.Model)
	}
	j.Name = fmt.Sprintf("projects/%s/locations/%s/batchPredictionJobs/%d", testProject, testLocation, 1000+len(f.jobs))
	j.State = string(genai.JobStatePending)
	j.CreateTime = created.Format(time.RFC3339)
	f.jobs = append(f.jobs, j)
	if f.dropCreateResponses > 0 {
		f.dropCreateResponses--
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":503,"message":"scripted lost response","status":"UNAVAILABLE"}}`)
		return
	}
	_ = json.NewEncoder(w).Encode(j)
}

// list answers one job per page so callers must follow page tokens.
func (f *fakeVertex) list(w io.Writer, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	f.filters = append(f.filters, filter)
	if f.raceJob != nil && len(f.filters) > f.raceAfterLists {
		f.jobs = append(f.jobs, f.raceJob)
		f.raceJob = nil
	}
	var matched []*vertexJob
	for _, j := range f.jobs {
		if j.Name == f.unlisted {
			continue
		}
		if f.ignoreFilter || filter == fmt.Sprintf("display_name=%q", j.DisplayName) {
			matched = append(matched, j)
		}
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	resp := map[string]any{}
	if start < len(matched) {
		resp["batchPredictionJobs"] = matched[start : start+1]
		if start+1 < len(matched) {
			resp["nextPageToken"] = strconv.Itoa(start + 1)
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// finish moves the job named name to state and writes lines as its output.
func (f *fakeVertex) finish(t *testing.T, gcs *fakeGCS, name string, state genai.JobState, lines ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.job("/v1beta1/" + name)
	if j == nil {
		t.Fatalf("no job %q", name)
	}
	j.State = string(state)
	dir := j.OutputConfig.GCSDestination.OutputURIPrefix + "prediction-model-2026-10-18T00:00:00Z"
	j.OutputInfo = map[string]string{"gcsOutputDirectory": dir}
	j.CompletionStats = map[string]string{"successfulCount": strconv.Itoa(len(lines)), "failedCount": "0", "incompleteCount": "0"}
	if len(lines) > 0 {
		object := strings.TrimPrefix(dir, "gs://"+testBucket+"/") + "/predictions.jsonl"
		gcs.put(object, []byte(strings.Join(lines, "\n")+"\n"))
	}
}

// outputDir returns the output directory Vertex reports for the job named name.
func (f *fakeVertex) outputDir(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.job("/v1beta1/" + name).OutputInfo["gcsOutputDirectory"]
}

// setOutputDir overrides the output directory Vertex reports for the job
// named name.
func (f *fakeVertex) setOutputDir(name, dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.job("/v1beta1/" + name).OutputInfo["gcsOutputDirectory"] = dir
}

// states returns each job's state by name.
func (f *fakeVertex) states() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.jobs))
	for _, j := range f.jobs {
		out[j.Name] = j.State
	}
	return out
}

// duplicate lists a copy of the job named name under a new name and
// CreateTime, as a second submitter's create would leave, and returns the
// new name.
func (f *fakeVertex) duplicate(t *testing.T, name string, n int, createTime string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.job("/v1beta1/" + name)
	if j == nil {
		t.Fatalf("no job %q", name)
	}
	dup := *j
	dup.Name = fmt.Sprintf("projects/%s/locations/%s/batchPredictionJobs/%d", testProject, testLocation, n)
	dup.CreateTime = createTime
	f.jobs = append(f.jobs, &dup)
	return dup.Name
}

// setCreateTime overrides the CreateTime of the job named name.
func (f *fakeVertex) setCreateTime(name, createTime string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.job("/v1beta1/" + name).CreateTime = createTime
}

func (f *fakeVertex) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

// testEnv is a Client wired to fresh fakes.
type testEnv struct {
	client *Client
	gcs    *fakeGCS
	vertex *fakeVertex
}

func fastRetry() retry.RetryConfig {
	return retry.RetryConfig{MaxRetries: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
}

func newTestEnv(t *testing.T, opts ...Option) *testEnv {
	t.Helper()
	gcs := &fakeGCS{objects: map[string][]byte{}}
	gcsSrv := httptest.NewServer(gcs)
	t.Cleanup(gcsSrv.Close)
	sc, err := storage.NewClient(t.Context(),
		option.WithEndpoint(gcsSrv.URL+"/storage/v1/"),
		option.WithoutAuthentication(),
		storage.WithJSONReads())
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	// The client's own retries would hide the scripted failures.
	sc.SetRetry(storage.WithPolicy(storage.RetryNever))

	vertex := &fakeVertex{}
	vSrv := httptest.NewServer(vertex)
	t.Cleanup(vSrv.Close)
	gc, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     testProject,
		Location:    testLocation,
		HTTPClient:  vSrv.Client(),
		HTTPOptions: genai.HTTPOptions{BaseURL: vSrv.URL},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	c, err := NewClient(gc, sc.Bucket(testBucket), testPrefix, append([]Option{WithRetryConfig(fastRetry())}, opts...)...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &testEnv{client: c, gcs: gcs, vertex: vertex}
}

// jsonLines decodes a JSONL object.
func jsonLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range bytes.Lines(data) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}
