/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"chainguard.dev/driftlessaf/agents/executor/retry"
)

// anthropicVersion is the anthropic_version Vertex requires on every Claude
// request body.
const anthropicVersion = "vertex-2023-10-16"

// keyLabel is the Gemini request label that carries a line's reference.
const keyLabel = "driftlessaf_batch_key"

// ClaudeRequest is one Claude Messages request in a batch.
type ClaudeRequest struct {
	// Key identifies the request in [Result.Key]; it must be unique in the
	// job.
	Key string
	// Params is the request. Its Model is ignored, since the job carries the
	// model, and streaming does not apply.
	Params anthropic.MessageNewParams
}

// GeminiRequest is one Gemini generateContent request in a batch.
type GeminiRequest struct {
	// Key identifies the request in [Result.Key]; it must be unique in the
	// job.
	Key string
	// Contents is the conversation sent to the model.
	Contents []*genai.Content
	// Config holds the generation settings and request-level fields such as
	// the system instruction, safety settings, and labels. Client-side
	// fields (HTTPOptions, automatic continuation) are not sent.
	Config *genai.GenerateContentConfig
}

// SubmitOption configures one submission. None are defined yet; the
// parameter keeps later options from changing the Submit signatures.
type SubmitOption func(*submitConfig) error

type submitConfig struct{}

// encodedLine is one input line and the manifest entry that finds its
// output again.
type encodedLine struct {
	line  []byte
	entry manifestEntry
}

// SubmitClaude submits reqs to model (a bare Vertex model ID such as
// "claude-opus-5-5@default") as one batch job under the submission id.
// Submitting an id that already has a job returns that job without writing
// input or creating another; a new id is needed to resubmit the same rows.
// Different rows under an id with no job yet are rejected with
// [ErrInvalidRequest]. Concurrent calls with the same id and rows return the
// job recorded first, though a duplicate may be created and then cancelled;
// see the package documentation.
func (c *Client) SubmitClaude(ctx context.Context, id, model string, reqs []ClaudeRequest, opts ...SubmitOption) (Job, error) {
	if err := submitOptions(opts); err != nil {
		return Job{}, err
	}
	lines := make([]encodedLine, 0, len(reqs))
	for _, r := range reqs {
		l, err := encodeClaude(r)
		if err != nil {
			return Job{}, fmt.Errorf("%w: key %q: %w", ErrInvalidRequest, r.Key, err)
		}
		lines = append(lines, l)
	}
	return c.submit(ctx, id, providerAnthropic, model, lines)
}

// SubmitGemini submits reqs to model (a bare Vertex model ID such as
// "gemini-3.8-flash") as one batch job under the submission id. It shares
// [Client.SubmitClaude]'s submission identity contract.
func (c *Client) SubmitGemini(ctx context.Context, id, model string, reqs []GeminiRequest, opts ...SubmitOption) (Job, error) {
	if err := submitOptions(opts); err != nil {
		return Job{}, err
	}
	lines := make([]encodedLine, 0, len(reqs))
	for _, r := range reqs {
		l, err := encodeGemini(r)
		if err != nil {
			return Job{}, fmt.Errorf("%w: key %q: %w", ErrInvalidRequest, r.Key, err)
		}
		lines = append(lines, l)
	}
	return c.submit(ctx, id, providerGoogle, model, lines)
}

func submitOptions(opts []SubmitOption) error {
	var cfg submitConfig
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	return nil
}

func (c *Client) submit(ctx context.Context, id, provider, model string, lines []encodedLine) (Job, error) {
	if !idPattern.MatchString(id) {
		return Job{}, fmt.Errorf("%w: id %q must match %s", ErrInvalidRequest, id, idPattern)
	}
	if model == "" || strings.Contains(model, "/") {
		return Job{}, fmt.Errorf("%w: model %q must be a bare model ID", ErrInvalidRequest, model)
	}
	if len(lines) == 0 {
		return Job{}, fmt.Errorf("%w: no requests", ErrInvalidRequest)
	}
	m := manifest{Provider: provider, Model: model, Keys: make([]manifestEntry, 0, len(lines))}
	keys := make(map[string]struct{}, len(lines))
	refs := make(map[string]struct{}, len(lines))
	digest := sha256.New()
	for _, l := range lines {
		if _, dup := keys[l.entry.Key]; dup {
			return Job{}, fmt.Errorf("%w: duplicate key %q", ErrInvalidRequest, l.entry.Key)
		}
		if _, dup := refs[l.entry.Ref]; dup {
			return Job{}, fmt.Errorf("%w: key %q collides with another key's reference", ErrInvalidRequest, l.entry.Key)
		}
		keys[l.entry.Key], refs[l.entry.Ref] = struct{}{}, struct{}{}
		m.Keys = append(m.Keys, l.entry)
		digest.Write(l.line)
		digest.Write([]byte{'\n'})
	}
	m.InputSHA256 = hex.EncodeToString(digest.Sum(nil))

	bjs, err := c.lookup(ctx, id)
	if err != nil {
		return Job{}, err
	}
	chosen, err := c.recorded(ctx, id, bjs)
	if err != nil {
		return Job{}, err
	}
	var own *genai.BatchJob
	if chosen == nil {
		if len(bjs) > 0 {
			// A submitter stopped between create and its record; the next
			// one records the earliest listed job.
			own = bjs[0]
		} else {
			if err := c.stage(ctx, id, lines, m); err != nil {
				return Job{}, err
			}
			// Create carries no request id, so every attempt looks for the
			// job first: a retry after a lost response finds the job instead
			// of making a second one.
			own, err = retry.RetryWithBackoff(ctx, c.retry, "batch.create", retryable(ctx), func() (*genai.BatchJob, error) {
				bjs, err := c.matches(ctx, id)
				if err != nil {
					return nil, err
				}
				if len(bjs) > 0 {
					return bjs[0], nil
				}
				return c.genai.Batches.Create(ctx, genaiModel(provider, model),
					&genai.BatchJobSource{Format: "jsonl", GCSURI: []string{c.gsURI(c.objectName(id, inputObject))}},
					&genai.CreateBatchJobConfig{
						DisplayName: id,
						Dest: &genai.BatchJobDestination{
							Format: "jsonl",
							GCSURI: c.gsURI(c.objectName(id, outputDir)) + "/",
						},
					})
			})
			if err != nil {
				return Job{}, fmt.Errorf("creating batch job %q: %w", id, err)
			}
		}
		// The record is create-only, so concurrent submitters settle on
		// whichever recorded first. After a lost response it names own.
		record := c.objectName(id, jobObject)
		err = c.write(ctx, record, "application/json", true, func(w *bufio.Writer) error {
			return json.NewEncoder(w).Encode(jobRecord{Name: own.Name})
		})
		switch {
		case err == nil:
			chosen = own
		case preconditionFailed(err):
			if chosen, err = c.recorded(ctx, id, slices.Concat(bjs, []*genai.BatchJob{own})); err != nil {
				return Job{}, err
			}
			if chosen == nil {
				return Job{}, fmt.Errorf("%q exists but could not be read", record)
			}
		default:
			return Job{}, err
		}
		// List again on both paths: a twin that created after our lookup and
		// stopped before its record would otherwise leave a live duplicate.
		if relisted, err := c.lookup(ctx, id); err != nil {
			clog.WarnContext(ctx, "Failed to list batch jobs for duplicates", "id", id, "error", err)
		} else {
			bjs = relisted
		}
	}
	c.cancelDuplicates(ctx, chosen.Name, slices.Concat(bjs, []*genai.BatchJob{own}))
	stored, err := c.manifestFor(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return existing(c.jobFor(id, chosen.Name, stored), provider, model)
}

// lookup lists the jobs under id, earliest first, with retries.
func (c *Client) lookup(ctx context.Context, id string) ([]*genai.BatchJob, error) {
	bjs, err := retry.RetryWithBackoff(ctx, c.retry, "batch.find", retryable(ctx), func() ([]*genai.BatchJob, error) {
		return c.matches(ctx, id)
	})
	if err != nil {
		return nil, fmt.Errorf("finding batch job %q: %w", id, err)
	}
	return bjs, nil
}

// cancelDuplicates cancels every job in bjs other than keep, so they stop
// before billing for the same rows where Vertex allows. Failures are logged:
// the survivor is unchanged.
func (c *Client) cancelDuplicates(ctx context.Context, keep string, bjs []*genai.BatchJob) {
	seen := map[string]struct{}{keep: {}}
	for _, bj := range bjs {
		if bj == nil {
			continue
		}
		if _, dup := seen[bj.Name]; dup {
			continue
		}
		seen[bj.Name] = struct{}{}
		if bj.State == genai.JobStateCancelling || statusOf(bj).Terminal {
			continue
		}
		clog.WarnContext(ctx, "Cancelling duplicate batch job", "id", bj.DisplayName, "job", bj.Name, "kept", keep)
		if err := c.Cancel(ctx, Job{Name: bj.Name}); err != nil {
			clog.ErrorContext(ctx, "Failed to cancel duplicate batch job", "id", bj.DisplayName, "job", bj.Name, "error", err)
		}
	}
}

// existing rejects reuse of a submission id for a different provider or
// model.
func existing(job Job, provider, model string) (Job, error) {
	if job.Provider != provider || job.Model != model {
		return Job{}, fmt.Errorf("%w: id %q already names a %s job for %q", ErrInvalidRequest, job.ID, job.Provider, job.Model)
	}
	return job, nil
}

// genaiModel is the model argument genai maps to a Vertex publisher path.
// Only Claude needs its publisher named; genai defaults to Google.
func genaiModel(provider, model string) string {
	if provider == providerAnthropic {
		return "anthropic/" + model
	}
	return model
}

// stage writes the manifest create-only, then the input. The manifest claims
// the id for these rows: a different submission is rejected, and the same
// rows (a retry, or a concurrent twin) go on to the same input.
func (c *Client) stage(ctx context.Context, id string, lines []encodedLine, m manifest) error {
	manifestName := c.objectName(id, manifestObject)
	err := c.write(ctx, manifestName, "application/json", true, func(w *bufio.Writer) error {
		return json.NewEncoder(w).Encode(m)
	})
	claimed := err == nil
	if !claimed {
		if !preconditionFailed(err) {
			return err
		}
		stored, err := c.manifestFor(ctx, id)
		if err != nil {
			return err
		}
		if stored.Provider != m.Provider || stored.Model != m.Model || stored.InputSHA256 != m.InputSHA256 || !slices.Equal(stored.Keys, m.Keys) {
			return fmt.Errorf("%w: id %q is already staged with different requests", ErrInvalidRequest, id)
		}
	}
	// The digest check above is what keeps another caller's input intact, as
	// a twin writes the same bytes; create-only here is defense in depth.
	err = c.write(ctx, c.objectName(id, inputObject), "application/jsonl", !claimed, func(w *bufio.Writer) error {
		for _, l := range lines {
			if _, err := w.Write(l.line); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && (claimed || !preconditionFailed(err)) {
		return err
	}
	return nil
}

// write uploads one object, create-only when ifAbsent, with retries.
func (c *Client) write(ctx context.Context, object, contentType string, ifAbsent bool, encode func(*bufio.Writer) error) error {
	_, err := retry.RetryWithBackoff(ctx, c.retry, "batch.write", retryable(ctx), func() (struct{}, error) {
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		obj := c.bucket.Object(object)
		if ifAbsent {
			obj = obj.If(storage.Conditions{DoesNotExist: true})
		}
		w := obj.NewWriter(wctx)
		w.ContentType = contentType
		bw := bufio.NewWriter(w)
		if err := errors.Join(encode(bw), bw.Flush()); err != nil {
			// Cancelling before Close aborts the upload.
			cancel()
			return struct{}{}, errors.Join(err, w.Close())
		}
		return struct{}{}, w.Close()
	})
	if err != nil {
		return fmt.Errorf("writing %q: %w", object, err)
	}
	return nil
}

// preconditionFailed reports a write refused because the object exists,
// over either the JSON or the gRPC storage API.
func preconditionFailed(err error) bool {
	if e, ok := errors.AsType[*googleapi.Error](err); ok && e.Code == http.StatusPreconditionFailed {
		return true
	}
	return status.Code(err) == codes.FailedPrecondition
}

// ref derives a line reference from a caller key. It is 32 lowercase hex
// characters, valid both as a Claude custom_id and as a Vertex label value,
// whatever characters the key contains.
func ref(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func encodeClaude(r ClaudeRequest) (encodedLine, error) {
	if r.Key == "" {
		return encodedLine{}, errors.New("key must not be empty")
	}
	if r.Params.MaxTokens <= 0 {
		return encodedLine{}, errors.New("max_tokens must be positive")
	}
	if len(r.Params.Messages) == 0 {
		return encodedLine{}, errors.New("at least one message is required")
	}
	raw, err := json.Marshal(r.Params)
	if err != nil {
		return encodedLine{}, fmt.Errorf("encoding params: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return encodedLine{}, fmt.Errorf("decoding params: %w", err)
	}
	delete(body, "model")
	delete(body, "stream")
	body["anthropic_version"] = json.RawMessage(`"` + anthropicVersion + `"`)
	id := ref(r.Key)
	line, err := json.Marshal(struct {
		CustomID string                     `json:"custom_id"`
		Request  map[string]json.RawMessage `json:"request"`
	}{id, body})
	if err != nil {
		return encodedLine{}, fmt.Errorf("encoding line: %w", err)
	}
	return encodedLine{line: line, entry: manifestEntry{Key: r.Key, Ref: id}}, nil
}

// geminiRequestFields are the GenerateContentConfig fields Vertex takes at
// the top level of a generateContent request rather than in
// generationConfig, matching genai's own request conversion.
var geminiRequestFields = []string{
	"systemInstruction", "safetySettings", "tools", "toolConfig", "labels",
	"cachedContent", "modelArmorConfig", "serviceTier",
}

// geminiClientFields are GenerateContentConfig fields that never go on the
// wire.
var geminiClientFields = []string{"httpOptions", "continuationToken", "automaticContinuation"}

func encodeGemini(r GeminiRequest) (encodedLine, error) {
	if r.Key == "" {
		return encodedLine{}, errors.New("key must not be empty")
	}
	if len(r.Contents) == 0 {
		return encodedLine{}, errors.New("at least one content is required")
	}
	body := map[string]any{"contents": r.Contents}
	if r.Config != nil {
		if r.Config.EnableEnhancedCivicAnswers != nil {
			return encodedLine{}, errors.New("enableEnhancedCivicAnswers is not supported on Vertex")
		}
		if _, ok := r.Config.Labels[keyLabel]; ok {
			return encodedLine{}, fmt.Errorf("label %q is reserved", keyLabel)
		}
		raw, err := json.Marshal(r.Config)
		if err != nil {
			return encodedLine{}, fmt.Errorf("encoding config: %w", err)
		}
		var gen map[string]json.RawMessage
		if err := json.Unmarshal(raw, &gen); err != nil {
			return encodedLine{}, fmt.Errorf("decoding config: %w", err)
		}
		for _, f := range geminiClientFields {
			delete(gen, f)
		}
		for _, f := range geminiRequestFields {
			if v, ok := gen[f]; ok {
				body[f] = v
				delete(gen, f)
			}
		}
		if v, ok := gen["modelSelectionConfig"]; ok {
			gen["modelConfig"] = v
			delete(gen, "modelSelectionConfig")
		}
		if len(gen) > 0 {
			body["generationConfig"] = gen
		}
	}
	digest, err := requestDigest(body)
	if err != nil {
		return encodedLine{}, err
	}
	label := ref(r.Key)
	labels := map[string]string{keyLabel: label}
	if r.Config != nil {
		maps.Copy(labels, r.Config.Labels)
	}
	body["labels"] = labels
	line, err := json.Marshal(struct {
		Request map[string]any `json:"request"`
	}{body})
	if err != nil {
		return encodedLine{}, fmt.Errorf("encoding line: %w", err)
	}
	return encodedLine{line: line, entry: manifestEntry{Key: r.Key, Ref: label, RequestSHA256: digest}}, nil
}

// requestDigest is the sha256 of a Gemini request in canonical form: decoded
// to generic JSON and re-encoded, without labels. Field order, whitespace,
// number spelling, and which labels Vertex echoes do not change it.
func requestDigest(req any) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encoding request: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", fmt.Errorf("decoding request: %w", err)
	}
	delete(generic, "labels")
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", fmt.Errorf("encoding canonical request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
