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
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	"chainguard.dev/driftlessaf/agents/executor/retry"
)

// defaultAnthropicVersion is the anthropic_version Vertex requires on every
// Claude request body.
const defaultAnthropicVersion = "vertex-2023-10-16"

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

// SubmitOption configures one submission.
type SubmitOption func(*submitConfig) error

type submitConfig struct {
	anthropicVersion string
}

// WithAnthropicVersion overrides the anthropic_version stamped on each
// Claude request, which defaults to "vertex-2023-10-16".
func WithAnthropicVersion(version string) SubmitOption {
	return func(c *submitConfig) error {
		if version == "" {
			return errors.New("anthropic version must not be empty")
		}
		c.anthropicVersion = version
		return nil
	}
}

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
func (c *Client) SubmitClaude(ctx context.Context, id, model string, reqs []ClaudeRequest, opts ...SubmitOption) (Job, error) {
	cfg, err := submitOptions(opts)
	if err != nil {
		return Job{}, err
	}
	lines := make([]encodedLine, 0, len(reqs))
	for _, r := range reqs {
		l, err := encodeClaude(r, cfg.anthropicVersion)
		if err != nil {
			return Job{}, fmt.Errorf("%w: key %q: %w", ErrInvalidRequest, r.Key, err)
		}
		lines = append(lines, l)
	}
	return c.submit(ctx, id, providerAnthropic, model, lines)
}

// SubmitGemini submits reqs to model (a bare Vertex model ID such as
// "gemini-3.8-flash") as one batch job under the submission id. Submitting
// an id that already has a job returns that job without writing input or
// creating another.
func (c *Client) SubmitGemini(ctx context.Context, id, model string, reqs []GeminiRequest, opts ...SubmitOption) (Job, error) {
	if _, err := submitOptions(opts); err != nil {
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

func submitOptions(opts []SubmitOption) (submitConfig, error) {
	cfg := submitConfig{anthropicVersion: defaultAnthropicVersion}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return submitConfig{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	return cfg, nil
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
	for _, l := range lines {
		if _, dup := keys[l.entry.Key]; dup {
			return Job{}, fmt.Errorf("%w: duplicate key %q", ErrInvalidRequest, l.entry.Key)
		}
		if _, dup := refs[l.entry.Ref]; dup {
			return Job{}, fmt.Errorf("%w: key %q collides with another key's reference", ErrInvalidRequest, l.entry.Key)
		}
		keys[l.entry.Key], refs[l.entry.Ref] = struct{}{}, struct{}{}
		m.Keys = append(m.Keys, l.entry)
	}

	job, ok, err := c.Find(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if ok {
		return existing(job, provider, model)
	}
	if err := c.writeInput(ctx, id, lines, m); err != nil {
		return Job{}, err
	}

	// Create carries no request id, so every attempt looks for the job
	// first: a retry after a lost response finds the job instead of making
	// a second one.
	job, err = retry.RetryWithBackoff(ctx, c.retry, "batch.create", retryable(ctx), func() (Job, error) {
		job, ok, err := c.find(ctx, id)
		if err != nil || ok {
			return job, err
		}
		bj, err := c.genai.Batches.Create(ctx, genaiModel(provider, model),
			&genai.BatchJobSource{Format: "jsonl", GCSURI: []string{c.gsURI(c.objectName(id, inputObject))}},
			&genai.CreateBatchJobConfig{
				DisplayName: id,
				Dest: &genai.BatchJobDestination{
					Format: "jsonl",
					GCSURI: c.gsURI(c.objectName(id, outputDir)) + "/",
				},
			})
		if err != nil {
			return Job{}, err
		}
		return c.jobFor(id, bj.Name, m), nil
	})
	if err != nil {
		return Job{}, fmt.Errorf("creating batch job %q: %w", id, err)
	}
	return existing(job, provider, model)
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

// writeInput writes the JSONL input, then the manifest, overwriting any left
// by an earlier attempt that never created a job.
func (c *Client) writeInput(ctx context.Context, id string, lines []encodedLine, m manifest) error {
	write := func(object, contentType string, encode func(*bufio.Writer) error) error {
		_, err := retry.RetryWithBackoff(ctx, c.retry, "batch.write", retryable(ctx), func() (struct{}, error) {
			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			w := c.bucket.Object(object).NewWriter(wctx)
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
	if err := write(c.objectName(id, inputObject), "application/jsonl", func(w *bufio.Writer) error {
		for _, l := range lines {
			if _, err := w.Write(l.line); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return write(c.objectName(id, manifestObject), "application/json", func(w *bufio.Writer) error {
		return json.NewEncoder(w).Encode(m)
	})
}

// ref derives a line reference from a caller key. It is 32 lowercase hex
// characters, valid both as a Claude custom_id and as a Vertex label value,
// whatever characters the key contains.
func ref(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func encodeClaude(r ClaudeRequest, version string) (encodedLine, error) {
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
	version, err = jsonString(version)
	if err != nil {
		return encodedLine{}, err
	}
	body["anthropic_version"] = json.RawMessage(version)
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

func jsonString(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
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
// to generic JSON and re-encoded, with the correlation label removed. Field
// order, whitespace, and number spelling in Vertex's echo do not change it,
// and neither does whether Vertex echoes labels at all.
func requestDigest(req any) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encoding request: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", fmt.Errorf("decoding request: %w", err)
	}
	if labels, ok := generic["labels"].(map[string]any); ok {
		delete(labels, keyLabel)
		if len(labels) == 0 {
			delete(generic, "labels")
		}
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", fmt.Errorf("encoding canonical request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
