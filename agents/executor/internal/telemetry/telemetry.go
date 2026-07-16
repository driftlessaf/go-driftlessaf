/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package telemetry

import (
	"context"
	"strconv"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"go.opentelemetry.io/otel/attribute"
)

// Recorder emits the GenAI metrics common to the executor backends, stamping
// every recording with the executor's model name, resource labels, and
// gen_ai.provider.name. Methods are safe for concurrent use: the attribute
// slice is built once at construction with exact capacity and only ever read
// afterwards, so concurrent Executes never append onto a shared backing array.
type Recorder struct {
	genai *metrics.GenAI
	model string
	attrs []attribute.KeyValue

	// codeFromError maps a backend API error (nil for success) to the
	// HTTP-style response code recorded by RecordAPIRequest.
	codeFromError func(error) int
}

// NewRecorder builds a Recorder for one executor instance. provider is the
// OTel gen_ai.provider.name value for the serving backend (for example
// "gcp.vertex_ai", "anthropic", or "openai-compat"). codeFromError maps a
// backend API error to the HTTP-style response code recorded by
// RecordAPIRequest; backends that do not record genai.api.requests may pass
// nil and must not call RecordAPIRequest or WithAPIRequestCounter.
func NewRecorder(genai *metrics.GenAI, model, provider string, resourceLabels map[string]string, codeFromError func(error) int) *Recorder {
	attrs := make([]attribute.KeyValue, 0, len(resourceLabels)+1)
	for k, v := range resourceLabels {
		attrs = append(attrs, attribute.String(k, v))
	}
	attrs = append(attrs, attribute.String("gen_ai.provider.name", provider))
	return &Recorder{
		genai:         genai,
		model:         model,
		attrs:         attrs,
		codeFromError: codeFromError,
	}
}

// RecordTokens records prompt and completion token usage.
func (r *Recorder) RecordTokens(ctx context.Context, inputTokens, outputTokens int64) {
	r.genai.RecordTokens(ctx, r.model, inputTokens, outputTokens, r.attrs...)
}

// RecordCacheTokens records prompt cache token usage: tokens served from
// cache and tokens written to it.
func (r *Recorder) RecordCacheTokens(ctx context.Context, cacheRead, cacheCreation int64) {
	r.genai.RecordCacheTokens(ctx, r.model, cacheRead, cacheCreation, r.attrs...)
}

// RecordToolCall records a tool call metric.
func (r *Recorder) RecordToolCall(ctx context.Context, toolName string) {
	r.genai.RecordToolCall(ctx, r.model, toolName, r.attrs...)
}

// RecordTurns records the number of turns used and, when limitExceeded is true,
// increments the turn_limit_exceeded counter.
func (r *Recorder) RecordTurns(ctx context.Context, turns int, limitExceeded bool) {
	r.genai.RecordTurns(ctx, r.model, turns, limitExceeded, r.attrs...)
}

// RecordAPIRequest counts a single LLM API attempt with a response_code
// derived from err via the Recorder's codeFromError mapping. Call this after
// every retry-wrapped API call (whether the final outcome was success,
// retryable failure, or non-retryable failure) so the counter sees one
// increment per HTTP attempt — matching what GCP's serviceruntime metric sees
// on its side of the wire.
func (r *Recorder) RecordAPIRequest(ctx context.Context, err error) {
	r.genai.RecordAPIRequest(ctx, r.model, responseCodeAttr(r.codeFromError(err)), r.attrs...)
}

// WithAPIRequestCounter extends cfg.OnAttemptError to also count each retried
// API attempt in genai.api.requests. The retry loop only invokes
// OnAttemptError for retryable errors that will be retried, so this captures
// the intermediate attempts that retry.RetryWithBackoff would otherwise hide;
// the final attempt is counted by the caller after RetryWithBackoff returns.
// Together they give exactly one increment per HTTP attempt.
func (r *Recorder) WithAPIRequestCounter(ctx context.Context, cfg retry.RetryConfig) retry.RetryConfig {
	base := cfg.OnAttemptError
	cfg.OnAttemptError = func(err error) {
		if base != nil {
			base(err)
		}
		r.RecordAPIRequest(ctx, err)
	}
	return cfg
}

// responseCodeAttr formats a code from the codeFromError mapping as a string
// attribute for the genai.api.requests counter. Mirrors the response_code
// label on serviceruntime.googleapis.com/api/request_count: "200" for
// success, the numeric code for everything we recognise, "unknown" for
// errors that don't carry a status (so they still get counted).
func responseCodeAttr(code int) string {
	switch {
	case code == 0:
		return "200"
	case code < 0:
		return "unknown"
	default:
		return strconv.Itoa(code)
	}
}
