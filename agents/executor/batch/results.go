/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"google.golang.org/api/iterator"
	"google.golang.org/genai"

	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
)

// maxLineBytes bounds one output line. Vertex echoes the request on each
// line, so a line can be several times the size of the response.
const maxLineBytes = 256 << 20

// Result is the outcome of one request in a job.
type Result struct {
	// Key is the request's key as submitted.
	Key string
	// Text is the response text: Claude's text blocks, or the non-thought
	// text parts of Gemini's first candidate, concatenated in order.
	Text string
	// Usage is the token accounting for the request.
	Usage Usage
	// Stop is the provider's stop or finish reason, verbatim.
	Stop string
	// Refusal is set with [ErrRefused].
	Refusal *Refusal
	// Err is nil for a usable response, [ErrRefused], [ErrTruncated],
	// [ErrMissing], or a per-line provider error; [IsRetryable] reports
	// whether that last kind is transient.
	Err error
}

// Usage is a request's token accounting, with the same meaning as the online
// executors record: In includes cached input for Gemini, Out includes
// Gemini's thinking tokens.
type Usage struct {
	In, Out, CacheRead, CacheWrite int64
}

// Refusal is the provider's reason for refusing a request.
type Refusal struct {
	// Category is Claude's stop_details category, Gemini's prompt
	// blockReason, or Gemini's blocking finishReason.
	Category string
	// Detail is Claude's explanation, Gemini's blockReasonMessage, or
	// Gemini's finishMessage.
	Detail string
}

// geminiBlockingFinishReasons are the finish reasons that mean the response
// was withheld rather than finished.
var geminiBlockingFinishReasons = []genai.FinishReason{
	genai.FinishReasonSafety, genai.FinishReasonRecitation, genai.FinishReasonBlocklist,
	genai.FinishReasonProhibitedContent, genai.FinishReasonSPII, genai.FinishReasonImageSafety,
	genai.FinishReasonImageProhibitedContent, genai.FinishReasonImageRecitation,
}

// outputLine is the envelope of one prediction line. Both Vertex's
// request/response/status shape and Anthropic's custom_id/result shape are
// accepted.
type outputLine struct {
	CustomID string          `json:"custom_id"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
	Status   json.RawMessage `json:"status"`
	Error    json.RawMessage `json:"error"`
	Result   *struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	} `json:"result"`
}

// Results streams the job's results. Each output line yields one Result
// keyed by the caller's key; once the job is terminal, every key with no
// line then yields [ErrMissing], in submission order. Before the job is
// terminal it yields only the lines already written. A non-nil error ends
// the sequence: the job or its output could not be read.
//
// Lines that match no submitted key are skipped and logged, so they surface
// as [ErrMissing]. With [WithMetrics], usage is recorded as each result is
// yielded, so iterating twice records twice.
func (c *Client) Results(ctx context.Context, job Job) iter.Seq2[Result, error] {
	return func(yield func(Result, error) bool) {
		ctx := metrics.WithOperationName(ctx, operationName)
		m, err := c.manifestFor(ctx, job)
		if err != nil {
			yield(Result{}, err)
			return
		}
		bj, err := c.get(ctx, job)
		if err != nil {
			yield(Result{}, err)
			return
		}
		objects, err := c.outputObjects(ctx, job, bj)
		if err != nil {
			yield(Result{}, err)
			return
		}
		col := newCollector(m, c.recorder(job))
		for _, object := range objects {
			_, err := retry.RetryWithBackoff(ctx, c.retry, "batch.read", retryable(ctx), func() (struct{}, error) {
				return struct{}{}, c.readOutput(ctx, object, col, yield)
			})
			if errors.Is(err, errStopped) {
				return
			}
			if err != nil {
				yield(Result{}, fmt.Errorf("reading %q: %w", object, err))
				return
			}
		}
		if !statusOf(bj).Terminal {
			return
		}
		for _, e := range m.Keys {
			if _, ok := col.seen[e.Key]; !ok && !yield(Result{Key: e.Key, Err: ErrMissing}, nil) {
				return
			}
		}
	}
}

// errStopped reports that the consumer stopped the iteration.
var errStopped = errors.New("iteration stopped")

// outputObjects lists the prediction files of a job. Their names are not
// documented, so every object under the output directory is read.
func (c *Client) outputObjects(ctx context.Context, job Job, bj *genai.BatchJob) ([]string, error) {
	if bj.OutputInfo == nil || bj.OutputInfo.GCSOutputDirectory == "" {
		return nil, nil
	}
	dir, err := c.objectFromURI(strings.TrimSuffix(bj.OutputInfo.GCSOutputDirectory, "/") + "/")
	if err != nil {
		return nil, err
	}
	want, err := c.objectFromURI(job.OutputPrefix)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(dir, want) {
		return nil, fmt.Errorf("output directory %q is outside the job's output prefix %q", bj.OutputInfo.GCSOutputDirectory, job.OutputPrefix)
	}
	return retry.RetryWithBackoff(ctx, c.retry, "batch.list", retryable(ctx), func() ([]string, error) {
		var names []string
		it := c.bucket.Objects(ctx, &storage.Query{Prefix: dir})
		for {
			attrs, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return names, nil
			}
			if err != nil {
				return nil, err
			}
			names = append(names, attrs.Name)
		}
	})
}

// readOutput yields the results in one output object. A retried read skips
// keys an earlier attempt already yielded.
func (c *Client) readOutput(ctx context.Context, object string, col *collector, yield func(Result, error) bool) error {
	r, err := c.bucket.Object(object).NewReader(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		res, ok := col.parse(ctx, object, sc.Bytes())
		if !ok {
			continue
		}
		col.record(ctx, res)
		if !yield(res, nil) {
			return errStopped
		}
	}
	return sc.Err()
}

// collector correlates output lines with manifest keys.
type collector struct {
	provider string
	byRef    map[string]string
	// byDigest queues the keys per request digest; identical requests are
	// interchangeable, so they are assigned in submission order.
	byDigest map[string][]string
	seen     map[string]struct{}
	rec      *telemetry.Recorder
}

func newCollector(m manifest, rec *telemetry.Recorder) *collector {
	col := &collector{
		provider: m.Provider,
		byRef:    make(map[string]string, len(m.Keys)),
		byDigest: make(map[string][]string, len(m.Keys)),
		seen:     make(map[string]struct{}, len(m.Keys)),
		rec:      rec,
	}
	for _, e := range m.Keys {
		col.byRef[e.Ref] = e.Key
		if e.RequestSHA256 != "" {
			col.byDigest[e.RequestSHA256] = append(col.byDigest[e.RequestSHA256], e.Key)
		}
	}
	return col
}

// parse decodes one line into a Result. It reports false for a line that is
// malformed, matches no key, or repeats a key already yielded.
func (col *collector) parse(ctx context.Context, object string, raw []byte) (Result, bool) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Result{}, false
	}
	var line outputLine
	if err := json.Unmarshal(raw, &line); err != nil {
		clog.WarnContext(ctx, "Skipping malformed batch output line", "object", object, "error", err)
		return Result{}, false
	}
	key, ok := col.key(line)
	if !ok {
		clog.WarnContext(ctx, "Skipping batch output line that matches no submitted key", "object", object)
		return Result{}, false
	}
	if _, dup := col.seen[key]; dup {
		return Result{}, false
	}
	col.seen[key] = struct{}{}
	if col.provider == providerAnthropic {
		return claudeResult(key, line), true
	}
	return geminiResult(key, line), true
}

func (col *collector) key(line outputLine) (string, bool) {
	if col.provider == providerAnthropic {
		key, ok := col.byRef[line.CustomID]
		return key, ok
	}
	var req map[string]any
	if err := json.Unmarshal(line.Request, &req); err != nil {
		return "", false
	}
	if labels, ok := req["labels"].(map[string]any); ok {
		if label, ok := labels[keyLabel].(string); ok {
			if key, ok := col.byRef[label]; ok {
				return key, true
			}
		}
	}
	digest, err := requestDigest(req)
	if err != nil {
		return "", false
	}
	for i, key := range col.byDigest[digest] {
		if _, dup := col.seen[key]; !dup {
			col.byDigest[digest] = col.byDigest[digest][i+1:]
			return key, true
		}
	}
	return "", false
}

// record emits metrics for a yielded result. A refusal or truncation is a
// completed request; any usage a failed line reports was still billed.
func (col *collector) record(ctx context.Context, res Result) {
	if col.rec == nil {
		return
	}
	var reqErr error
	if _, ok := errors.AsType[*lineError](res.Err); ok {
		reqErr = res.Err
	}
	col.rec.RecordAPIRequest(ctx, reqErr)
	if res.Usage != (Usage{}) {
		col.rec.RecordTokens(ctx, res.Usage.In, res.Usage.Out)
		col.rec.RecordCacheTokens(ctx, res.Usage.CacheRead, res.Usage.CacheWrite)
	}
}

// lineFailure decodes a line's error, from a status string or an error
// object, or returns nil.
func lineFailure(fields ...json.RawMessage) *lineError {
	for _, raw := range fields {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s == "" {
				continue
			}
			return &lineError{code: codeFromMessage(s), message: s}
		}
		var obj struct {
			Code    int    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			return &lineError{code: -1, message: string(raw)}
		}
		if obj.Error != nil {
			obj.Type, obj.Message = obj.Error.Type, obj.Error.Message
		}
		msg := strings.TrimSpace(obj.Type + ": " + obj.Message)
		code := obj.Code
		if code == 0 {
			code = codeFromMessage(msg)
		}
		return &lineError{code: code, message: msg}
	}
	return nil
}

func claudeResult(key string, line outputLine) Result {
	res := Result{Key: key}
	body := line.Response
	if r := line.Result; r != nil {
		switch r.Type {
		case "succeeded":
			body = r.Message
		case "errored":
			res.Err = lineFailure(r.Error)
		default:
			// expired or canceled: the request never ran.
			res.Err = &lineError{code: -1, message: "request " + r.Type, transient: true}
		}
	}
	if res.Err != nil {
		return res
	}
	if e := lineFailure(line.Status, line.Error); e != nil {
		res.Err = e
		return res
	}
	var msg anthropic.Message
	if err := json.Unmarshal(body, &msg); err != nil || len(body) == 0 {
		res.Err = &lineError{code: -1, message: "line has no decodable response"}
		return res
	}
	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	res.Text = text.String()
	res.Stop = string(msg.StopReason)
	res.Usage = Usage{
		In:         msg.Usage.InputTokens,
		Out:        msg.Usage.OutputTokens,
		CacheRead:  msg.Usage.CacheReadInputTokens,
		CacheWrite: msg.Usage.CacheCreationInputTokens,
	}
	switch msg.StopReason {
	case anthropic.StopReasonRefusal:
		res.Refusal = &Refusal{Category: string(msg.StopDetails.Category), Detail: msg.StopDetails.Explanation}
		res.Err = ErrRefused
	case anthropic.StopReasonMaxTokens:
		res.Err = ErrTruncated
	}
	return res
}

func geminiResult(key string, line outputLine) Result {
	res := Result{Key: key}
	if e := lineFailure(line.Status, line.Error); e != nil {
		res.Err = e
		return res
	}
	var resp genai.GenerateContentResponse
	if err := json.Unmarshal(line.Response, &resp); err != nil || len(line.Response) == 0 {
		res.Err = &lineError{code: -1, message: "line has no decodable response"}
		return res
	}
	if u := resp.UsageMetadata; u != nil {
		res.Usage = Usage{
			In:        int64(u.PromptTokenCount) + int64(u.ToolUsePromptTokenCount),
			Out:       int64(u.CandidatesTokenCount) + int64(u.ThoughtsTokenCount),
			CacheRead: int64(u.CachedContentTokenCount),
		}
	}
	if len(resp.Candidates) == 0 {
		if fb := resp.PromptFeedback; fb != nil && fb.BlockReason != "" {
			res.Stop = string(fb.BlockReason)
			res.Refusal = &Refusal{Category: string(fb.BlockReason), Detail: fb.BlockReasonMessage}
			res.Err = ErrRefused
			return res
		}
		// The online executor retries an empty turn as transient.
		res.Err = &lineError{code: -1, message: "response has no candidates", transient: true}
		return res
	}
	cand := resp.Candidates[0]
	res.Stop = string(cand.FinishReason)
	if cand.Content != nil {
		var text strings.Builder
		for _, part := range cand.Content.Parts {
			if part != nil && !part.Thought {
				text.WriteString(part.Text)
			}
		}
		res.Text = text.String()
	}
	switch {
	case slices.Contains(geminiBlockingFinishReasons, cand.FinishReason):
		res.Refusal = &Refusal{Category: string(cand.FinishReason), Detail: cand.FinishMessage}
		res.Err = ErrRefused
	case cand.FinishReason == genai.FinishReasonMaxTokens:
		res.Err = ErrTruncated
	}
	return res
}
