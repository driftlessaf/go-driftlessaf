/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// DefaultEndpoint is TypeSafe AI's hosted System One endpoint.
const DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"

// ProviderName is the gen_ai.provider.name value stamped on this client's
// metrics when no route supplies one.
const ProviderName = "typesafe"

// Model aliases published by TypeSafe AI. Exact versioned ids such as
// "jev-1.13.0" are also accepted; the alias set is the stable surface.
const (
	// ModelJevLatest is the most recent stable, official Jev release.
	ModelJevLatest = "jev-latest"
	// ModelJevPreview is the most recent Jev release, official or not.
	ModelJevPreview = "jev-preview"
)

// defaultMaxResponseBytes caps a response body; a well-formed answer set is
// kilobytes, so the cap only guards memory against a misbehaving upstream.
const defaultMaxResponseBytes = 16 << 20

// Request is one System One call.
type Request struct {
	// Model is the model id or alias, for example [ModelJevLatest]. It may
	// be empty on a client constructed with [WithRoute], which then sends
	// the route's provider model ID.
	Model string `json:"model"`
	// State is the content every question is evaluated against.
	State Content `json:"state"`
	// Questions maps caller-chosen ids to questions. Answers come back under
	// the same ids.
	Questions map[string]Question `json:"questions"`
}

// Validate reports whether r can be sent. Every failure wraps
// ErrInvalidRequest.
func (r Request) Validate() error {
	if r.Model == "" {
		return fmt.Errorf("%w: model must not be empty", ErrInvalidRequest)
	}
	if r.State == nil {
		return fmt.Errorf("%w: state must not be nil", ErrInvalidRequest)
	}
	if len(r.Questions) == 0 {
		return fmt.Errorf("%w: at least one question is required", ErrInvalidRequest)
	}
	for id, q := range r.Questions {
		if id == "" {
			return fmt.Errorf("%w: question id must not be empty", ErrInvalidRequest)
		}
		q, err := normalizeQuestion(q)
		if err != nil {
			return fmt.Errorf("%w: question %q: %w", ErrInvalidRequest, id, err)
		}
		if err := q.validate(); err != nil {
			return fmt.Errorf("%w: question %q: %w", ErrInvalidRequest, id, err)
		}
	}
	return nil
}

// Response is the answer set for one Request.
type Response struct {
	// Model is the model that served the request, with aliases resolved as
	// the API reports them.
	Model string
	// Answers holds one answer per question id in the request.
	Answers map[string]Answer
	// Usage is the token accounting for the call.
	Usage Usage
}

// Usage is the API's token accounting. Output tokens are reported but not
// billed by the provider.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type wireResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   Usage                      `json:"usage"`
}

// Client calls the System One API. Construct one with [NewClient]; a Client is
// safe for concurrent use once constructed.
type Client struct {
	endpoint         string
	apiKey           string
	httpClient       *http.Client
	retry            retry.RetryConfig
	genai            *metrics.GenAI
	resourceLabels   map[string]string
	maxResponseBytes int64
	// defaultModel and providerName come from a route when one is supplied.
	defaultModel string
	providerName string
}

// Option configures a Client.
type Option func(*Client) error

// WithEndpoint overrides the API endpoint. The URL must be absolute with an
// http or https scheme.
func WithEndpoint(endpoint string) Option {
	return func(c *Client) error {
		u, err := url.Parse(endpoint)
		if err != nil {
			return fmt.Errorf("endpoint: %w", err)
		}
		if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("endpoint %q must be an absolute http(s) URL", endpoint)
		}
		c.endpoint = endpoint
		return nil
	}
}

// WithRoute binds the client to a resolved route on the
// [modelrouter.ProtocolTypeSafeSystemOne] protocol. Requests that leave Model
// empty send the route's provider model ID, and metrics carry the route's
// provider attribution. Any other protocol is rejected, so a conversational
// route cannot be handed to this client by mistake.
func WithRoute(plan modelrouter.Plan) Option {
	return func(c *Client) error {
		if err := plan.Validate(); err != nil {
			return fmt.Errorf("route: %w", err)
		}
		if got := plan.Protocol(); got != modelrouter.ProtocolTypeSafeSystemOne {
			return fmt.Errorf("route: protocol %q is not %q", got, modelrouter.ProtocolTypeSafeSystemOne)
		}
		c.defaultModel = plan.ProviderModelID()
		c.providerName = plan.Attribution().ProviderName
		return nil
	}
}

// WithHTTPClient supplies the HTTP client, including any timeout or transport
// the caller wants. The default client has a 30-second timeout.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) error {
		if httpClient == nil {
			return errors.New("http client must not be nil")
		}
		c.httpClient = httpClient
		return nil
	}
}

// WithRetryConfig overrides the retry policy applied to retryable failures.
func WithRetryConfig(cfg retry.RetryConfig) Option {
	return func(c *Client) error {
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("retry config: %w", err)
		}
		c.retry = cfg
		return nil
	}
}

// WithMetrics records token usage and per-attempt request counts on genai.
// Without it the client records nothing.
func WithMetrics(genai *metrics.GenAI) Option {
	return func(c *Client) error {
		if genai == nil {
			return errors.New("metrics must not be nil")
		}
		c.genai = genai
		return nil
	}
}

// WithResourceLabels adds attributes to every metric the client records, for
// example service_name and agent_name.
func WithResourceLabels(labels map[string]string) Option {
	return func(c *Client) error {
		c.resourceLabels = labels
		return nil
	}
}

// WithMaxResponseBytes caps the response body the client is willing to read.
func WithMaxResponseBytes(limit int64) Option {
	return func(c *Client) error {
		if limit <= 0 {
			return errors.New("max response bytes must be positive")
		}
		c.maxResponseBytes = limit
		return nil
	}
}

// DefaultRetryConfig is the retry policy suited to the API's latency: a few
// short backoffs rather than the minute-scale quota backoffs of the
// conversational executors.
func DefaultRetryConfig() retry.RetryConfig {
	return retry.RetryConfig{
		MaxRetries:  2,
		BaseBackoff: 500 * time.Millisecond,
		MaxBackoff:  5 * time.Second,
		MaxJitter:   250 * time.Millisecond,
	}
}

// NewClient constructs a Client that authenticates with apiKey.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("api key must not be empty")
	}
	c := &Client{
		endpoint:         DefaultEndpoint,
		apiKey:           apiKey,
		httpClient:       &http.Client{Timeout: 30 * time.Second},
		retry:            DefaultRetryConfig(),
		maxResponseBytes: defaultMaxResponseBytes,
		providerName:     ProviderName,
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Ask sends req and returns its validated answers. Retryable failures are
// retried per the client's policy while ctx is live; the returned error is
// the last attempt's.
// Every non-2xx status surfaces as an *APIError, and a 2xx body that does not
// match the questions as sent surfaces as ErrResponseValidation.
func (c *Client) Ask(ctx context.Context, req Request) (*Response, error) {
	req.Model = cmp.Or(req.Model, c.defaultModel)
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding: %w", ErrInvalidRequest, err)
	}

	var recorder *telemetry.Recorder
	cfg := c.retry
	if c.genai != nil {
		recorder = telemetry.NewRecorder(c.genai, req.Model, c.providerName, c.resourceLabels, responseCode)
		cfg = recorder.WithAPIRequestCounter(ctx, cfg)
	}

	// A per-attempt HTTP timeout is retryable; an expired caller context is
	// not, even though both surface as timeout errors from net/http.
	retryable := func(err error) bool { return ctx.Err() == nil && IsRetryable(err) }
	resp, err := retry.RetryWithBackoff(ctx, cfg, "systemone.ask", retryable, func() (*Response, error) {
		return c.do(ctx, body, req.Questions)
	})
	if recorder != nil {
		recorder.RecordAPIRequest(ctx, err)
		if err == nil {
			recorder.RecordTokens(ctx, resp.Usage.InputTokens, resp.Usage.OutputTokens)
		}
	}
	return resp, err
}

// do performs one HTTP attempt.
func (c *Client) do(ctx context.Context, body []byte, questions map[string]Question) (*Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("system one: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, c.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("system one: reading response: %w", err)
	}
	if int64(len(raw)) > c.maxResponseBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrResponseTooLarge, c.maxResponseBytes)
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return nil, &APIError{
			StatusCode: httpResp.StatusCode,
			Body:       bodyExcerpt(raw),
			RetryAfter: parseRetryAfter(httpResp.Header.Get("Retry-After")),
		}
	}
	return decodeResponse(raw, questions)
}

// decodeResponse decodes a 2xx body and checks it against the questions as
// sent: every question has exactly one answer of its own kind and no answer
// arrives for a question that was not asked.
func decodeResponse(raw []byte, questions map[string]Question) (*Response, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, validationError("decoding body: %w", err)
	}
	resp := &Response{Model: w.Model, Usage: w.Usage, Answers: make(map[string]Answer, len(questions))}
	for id, question := range questions {
		rawAnswer, ok := w.Answers[id]
		if !ok {
			return nil, validationError("no answer for question %q", id)
		}
		answer, err := decodeAnswer(id, rawAnswer, question)
		if err != nil {
			return nil, err
		}
		resp.Answers[id] = answer
	}
	for id := range w.Answers {
		if _, ok := questions[id]; !ok {
			return nil, validationError("answer %q has no matching question", id)
		}
	}
	return resp, nil
}

// parseRetryAfter reads a Retry-After header given in seconds. HTTP-date
// forms and unparsable values yield zero.
func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
