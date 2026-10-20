/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/genai"

	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
)

const (
	providerAnthropic = "anthropic"
	providerGoogle    = "google"

	// telemetryProvider is the gen_ai.provider.name for both providers:
	// Vertex serves every batch job.
	telemetryProvider = "gcp.vertex_ai"
	operationName     = "batch_predict"

	inputObject    = "input.jsonl"
	manifestObject = "manifest.json"
	jobObject      = "job.json"
	outputDir      = "output/"

	// maxManifestBytes bounds the manifest read; 200,000 keys (the Vertex
	// per-job limit) fit well under it.
	maxManifestBytes = 256 << 20
	// maxRecordBytes bounds the job.json read, which holds one resource name.
	maxRecordBytes = 64 << 10
)

// idPattern keeps a submission id safe as a Vertex display name, a list
// filter literal, and a single object path segment.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

// Client submits and collects Vertex AI batch prediction jobs for Claude and
// Gemini. Construct one with [NewClient]; a Client is safe for concurrent use.
type Client struct {
	genai          *genai.Client
	bucket         *storage.BucketHandle
	prefix         string
	retry          retry.RetryConfig
	metrics        *metrics.GenAI
	resourceLabels map[string]string
}

// Option configures a Client.
type Option func(*Client) error

// WithRetryConfig overrides the retry policy for Vertex and Cloud Storage
// calls.
func WithRetryConfig(cfg retry.RetryConfig) Option {
	return func(c *Client) error {
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("retry config: %w", err)
		}
		c.retry = cfg
		return nil
	}
}

// WithMetrics records the token usage and outcome of every result
// [Client.Results] yields. Without it the client records nothing.
func WithMetrics(genai *metrics.GenAI) Option {
	return func(c *Client) error {
		if genai == nil {
			return errors.New("metrics must not be nil")
		}
		c.metrics = genai
		return nil
	}
}

// WithResourceLabels adds attributes to every metric the client records, for
// example service_name.
func WithResourceLabels(labels map[string]string) Option {
	return func(c *Client) error {
		c.resourceLabels = labels
		return nil
	}
}

// NewClient returns a Client that runs jobs through gc, which must use the
// Vertex AI backend, and stages input and output under prefix in bucket.
// The Vertex AI service agent needs read and create access to the bucket.
func NewClient(gc *genai.Client, bucket *storage.BucketHandle, prefix string, opts ...Option) (*Client, error) {
	if gc == nil {
		return nil, errors.New("genai client must not be nil")
	}
	if gc.ClientConfig().Backend != genai.BackendVertexAI {
		return nil, errors.New("batch prediction requires the Vertex AI backend")
	}
	if bucket == nil {
		return nil, errors.New("bucket must not be nil")
	}
	c := &Client{
		genai:  gc,
		bucket: bucket,
		prefix: strings.Trim(prefix, "/"),
		retry:  retry.DefaultRetryConfig(),
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Job identifies one submitted batch job. It holds everything [Client.Status],
// [Client.Results], and [Client.Cancel] need, so a caller may persist it.
type Job struct {
	// ID is the caller's submission id, also the job's display name.
	ID string
	// Name is the Vertex resource name of the BatchPredictionJob.
	Name string
	// Provider is "anthropic" for Claude jobs and "google" for Gemini jobs.
	Provider string
	// Model is the bare model ID as submitted, without a publisher prefix.
	Model string
	// InputURI is the gs:// URI of the job's JSONL input.
	InputURI string
	// OutputPrefix is the gs:// prefix Vertex writes predictions under.
	OutputPrefix string
	// Keys is the number of requests in the job.
	Keys int
}

// Status is a job's progress as Vertex reports it.
type Status struct {
	// State is the Vertex job state.
	State genai.JobState
	// Terminal reports that the job will make no further progress: it
	// succeeded, partially succeeded, failed, was cancelled, expired, or was
	// paused.
	Terminal bool
	// Succeeded, Failed, and Incomplete are the Vertex completion counts.
	// Incomplete is -1 when Vertex does not know it.
	Succeeded, Failed, Incomplete int
}

// manifest is written next to the input and maps each caller key to the
// identifiers its output line can be matched by.
type manifest struct {
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Keys     []manifestEntry `json:"keys"`
	// InputSHA256 is the digest of the input object, which tells a retry of
	// the same rows from another submission under the id.
	InputSHA256 string `json:"input_sha256"`
}

// jobRecord is written create-only once a job exists and names the job
// every submission of the id settles on.
type jobRecord struct {
	Name string `json:"name"`
}

type manifestEntry struct {
	// Key is the caller's key.
	Key string `json:"key"`
	// Ref is the custom_id (Claude) or label value (Gemini) on the line.
	Ref string `json:"ref"`
	// RequestSHA256 is the canonical request digest Gemini lines fall back
	// to when Vertex does not echo the label.
	RequestSHA256 string `json:"request_sha256,omitempty"`
}

// objectName returns the object path for name under the submission id.
func (c *Client) objectName(id, name string) string {
	return path.Join(c.prefix, id, name)
}

// gsURI renders an object path in this client's bucket as a gs:// URI.
func (c *Client) gsURI(object string) string {
	return "gs://" + c.bucket.BucketName() + "/" + object
}

// objectFromURI returns the object path of uri, which must name this
// client's bucket.
func (c *Client) objectFromURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, "gs://"+c.bucket.BucketName()+"/")
	if !ok {
		return "", fmt.Errorf("%q is not in bucket %q", uri, c.bucket.BucketName())
	}
	return rest, nil
}

// retryable is the retry predicate for every call: transient failures only,
// and never once the caller's context is done.
func retryable(ctx context.Context) func(error) bool {
	return func(err error) bool {
		return ctx.Err() == nil && IsRetryable(err)
	}
}

// Find returns the job submitted under id, if any. It filters the job list
// on the display name and re-checks the name and input URI on each job, so a
// filter Vertex ignores, or a job with the same name from another client,
// cannot produce a false match. A job with this id's name and input but
// another output destination, or none, or another model than the id was
// submitted for, was not created by this client: Find returns an error
// wrapping [ErrInvalidRequest] rather than adopt it.
//
// Find returns the job Submit recorded for the id, whatever its state and
// whatever duplicates are listed. A recorded job that does not exist or fails
// the checks above is also [ErrInvalidRequest]. With no record yet (a
// submitter stopped between creating the job and recording it), Find returns
// the earliest listed job, the one a recovering Submit records. Find is
// read-only: duplicates are cancelled only by Submit.
func (c *Client) Find(ctx context.Context, id string) (Job, bool, error) {
	if !idPattern.MatchString(id) {
		return Job{}, false, fmt.Errorf("%w: id %q must match %s", ErrInvalidRequest, id, idPattern)
	}
	m, err := c.manifestFor(ctx, id)
	staged := !errors.Is(err, storage.ErrObjectNotExist)
	if staged && err != nil {
		return Job{}, false, fmt.Errorf("finding batch job %q: %w", id, err)
	}
	// Submit stages the manifest before any create, so with none the id has
	// no model and every job claiming its input is a collision.
	var model string
	if staged {
		model = publisherModel(m.Provider, m.Model)
	}
	bjs, err := c.lookup(ctx, id, model)
	if err != nil {
		return Job{}, false, err
	}
	chosen, err := c.recorded(ctx, id, model, bjs)
	if err != nil {
		return Job{}, false, fmt.Errorf("finding batch job %q: %w", id, err)
	}
	if chosen == nil {
		if len(bjs) == 0 {
			return Job{}, false, nil
		}
		chosen = bjs[0]
	}
	if !staged {
		return Job{}, false, fmt.Errorf("%w: job %q claims id %q, which has no manifest", ErrInvalidRequest, chosen.Name, id)
	}
	return c.jobFor(id, chosen.Name, m), true, nil
}

// matches lists the jobs this client created under id for model, earliest
// first.
func (c *Client) matches(ctx context.Context, id, model string) ([]*genai.BatchJob, error) {
	page, err := c.genai.Batches.List(ctx, &genai.ListBatchJobsConfig{
		Filter: fmt.Sprintf("display_name=%q", id),
	})
	var out []*genai.BatchJob
	for ; err == nil; page, err = page.Next(ctx) {
		for _, bj := range page.Items {
			ok, err := c.owns(id, model, bj)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, bj)
			}
		}
	}
	if !errors.Is(err, genai.ErrPageDone) {
		return nil, err
	}
	slices.SortFunc(out, func(a, b *genai.BatchJob) int {
		return cmp.Or(a.CreateTime.Compare(b.CreateTime), strings.Compare(a.Name, b.Name))
	})
	return out, nil
}

// owns reports whether bj is a job this client created under id for model,
// a [publisherModel] path.
func (c *Client) owns(id, model string, bj *genai.BatchJob) (bool, error) {
	inputURI := c.gsURI(c.objectName(id, inputObject))
	outputURI := c.gsURI(c.objectName(id, outputDir))
	if bj.DisplayName != id || bj.Src == nil || !slices.Contains(bj.Src.GCSURI, inputURI) {
		return false, nil
	}
	// Our input is already in that job, so neither adopting it nor creating
	// a second job is safe.
	if bj.Dest == nil || strings.TrimSuffix(bj.Dest.GCSURI, "/") != outputURI {
		var dest string
		if bj.Dest != nil {
			dest = bj.Dest.GCSURI
		}
		return false, fmt.Errorf("%w: job %q reads this id's input but writes to %q, not %q",
			ErrInvalidRequest, bj.Name, dest, outputURI+"/")
	}
	// Like a foreign destination: the job reads our input, so skipping it
	// would leave its output from another model beside a second job.
	if got := resourceModel(bj.Model); got != model {
		return false, fmt.Errorf("%w: job %q reads this id's input but runs model %q, not %q",
			ErrInvalidRequest, bj.Name, bj.Model, model)
	}
	return true, nil
}

// publisherModel is the Vertex publisher model path genai creates a job with
// from [genaiModel].
func publisherModel(provider, model string) string {
	return "publishers/" + provider + "/models/" + model
}

// resourceModel strips the projects/*/locations/* prefix Vertex may report a
// publisher model under.
func resourceModel(model string) string {
	if parts := strings.SplitN(model, "/", 5); len(parts) == 5 && parts[0] == "projects" && parts[2] == "locations" {
		return parts[4]
	}
	return model
}

// recorded returns the job the id's job.json names, or nil when there is no
// record yet. The record is trusted like the manifest, but the job it names
// must still pass [Client.owns]; anything else is a collision.
func (c *Client) recorded(ctx context.Context, id, model string, bjs []*genai.BatchJob) (*genai.BatchJob, error) {
	object := c.objectName(id, jobObject)
	r, err := retry.RetryWithBackoff(ctx, c.retry, "batch.record", retryable(ctx), func() (*jobRecord, error) {
		rd, err := c.bucket.Object(object).NewReader(ctx)
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		defer rd.Close()
		var r jobRecord
		if err := json.NewDecoder(io.LimitReader(rd, maxRecordBytes)).Decode(&r); err != nil {
			return nil, fmt.Errorf("decoding %q: %w", object, err)
		}
		return &r, nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", object, err)
	}
	if r == nil {
		return nil, nil
	}
	if i := slices.IndexFunc(bjs, func(bj *genai.BatchJob) bool { return bj.Name == r.Name }); i >= 0 {
		return bjs[i], nil
	}
	// The list may not show the recorded job yet, so look it up by name.
	bj, err := c.get(ctx, Job{Name: r.Name})
	if responseCode(err) == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %q names job %q, which does not exist", ErrInvalidRequest, object, r.Name)
	}
	if err != nil {
		return nil, err
	}
	ok, err := c.owns(id, model, bj)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q names job %q, which was not created for id %q", ErrInvalidRequest, object, r.Name, id)
	}
	return bj, nil
}

// jobFor assembles the Job for a submission id from its Vertex name and
// manifest.
func (c *Client) jobFor(id, name string, m manifest) Job {
	return Job{
		ID:           id,
		Name:         name,
		Provider:     m.Provider,
		Model:        m.Model,
		InputURI:     c.gsURI(c.objectName(id, inputObject)),
		OutputPrefix: c.gsURI(c.objectName(id, outputDir)) + "/",
		Keys:         len(m.Keys),
	}
}

func (c *Client) readManifest(ctx context.Context, object string) (manifest, error) {
	r, err := c.bucket.Object(object).NewReader(ctx)
	if err != nil {
		return manifest{}, fmt.Errorf("reading manifest %q: %w", object, err)
	}
	defer r.Close()
	var m manifest
	if err := json.NewDecoder(io.LimitReader(r, maxManifestBytes)).Decode(&m); err != nil {
		return manifest{}, fmt.Errorf("decoding manifest %q: %w", object, err)
	}
	return m, nil
}

// manifestFor reads the manifest under a validated id; a persisted Job's
// URIs are not trusted to locate it.
func (c *Client) manifestFor(ctx context.Context, id string) (manifest, error) {
	return retry.RetryWithBackoff(ctx, c.retry, "batch.manifest", retryable(ctx), func() (manifest, error) {
		return c.readManifest(ctx, c.objectName(id, manifestObject))
	})
}

func (c *Client) get(ctx context.Context, job Job) (*genai.BatchJob, error) {
	if job.Name == "" {
		return nil, fmt.Errorf("%w: job has no name", ErrInvalidRequest)
	}
	bj, err := retry.RetryWithBackoff(ctx, c.retry, "batch.get", retryable(ctx), func() (*genai.BatchJob, error) {
		return c.genai.Batches.Get(ctx, job.Name, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("getting batch job %q: %w", job.Name, err)
	}
	return bj, nil
}

// Status returns the job's current state and completion counts.
func (c *Client) Status(ctx context.Context, job Job) (Status, error) {
	bj, err := c.get(ctx, job)
	if err != nil {
		return Status{}, err
	}
	return statusOf(bj), nil
}

func statusOf(bj *genai.BatchJob) Status {
	s := Status{State: bj.State}
	switch bj.State {
	case genai.JobStateSucceeded, genai.JobStatePartiallySucceeded, genai.JobStateFailed,
		genai.JobStateCancelled, genai.JobStateExpired, genai.JobStatePaused:
		s.Terminal = true
	}
	if cs := bj.CompletionStats; cs != nil {
		s.Succeeded, s.Failed, s.Incomplete = int(cs.SuccessfulCount), int(cs.FailedCount), int(cs.IncompleteCount)
	}
	return s
}

// Cancel asks Vertex to cancel the job. Cancellation is asynchronous: the
// job reaches CANCELLED later, and rows already finished keep their output.
func (c *Client) Cancel(ctx context.Context, job Job) error {
	if job.Name == "" {
		return fmt.Errorf("%w: job has no name", ErrInvalidRequest)
	}
	_, err := retry.RetryWithBackoff(ctx, c.retry, "batch.cancel", retryable(ctx), func() (struct{}, error) {
		return struct{}{}, c.genai.Batches.Cancel(ctx, job.Name, nil)
	})
	if err != nil {
		return fmt.Errorf("cancelling batch job %q: %w", job.Name, err)
	}
	return nil
}

// recorder returns the metrics recorder for job, or nil without
// [WithMetrics].
func (c *Client) recorder(job Job) *telemetry.Recorder {
	if c.metrics == nil {
		return nil
	}
	return telemetry.NewRecorder(c.metrics, job.Model, "", telemetryProvider, c.resourceLabels, responseCode)
}
