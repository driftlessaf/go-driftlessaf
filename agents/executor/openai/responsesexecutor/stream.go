/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
)

const maxPayloadBytes = 8 << 20

// httpFailure deliberately excludes provider bodies, URLs, and headers, which
// may contain secrets. Preserve only the status needed by retry and telemetry.
type httpFailure struct{ status int }

func (e *httpFailure) Error() string {
	return fmt.Sprintf("responses request failed (HTTP %d)", e.status)
}
func statusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if e, ok := errors.AsType[*httpFailure](err); ok {
		return e.status
	}
	return -1
}
func retryable(err error) bool {
	switch statusCode(err) {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		// Distinguish an exactly bounded body from an oversized one without
		// returning bytes beyond the limit to the decoder.
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errors.New("responses body exceeds byte limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func boundWire(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	if req.ContentLength < 0 || req.ContentLength > maxPayloadBytes {
		return nil, errors.New("responses request exceeds byte limit")
	}
	resp, err := next(req)
	if err != nil {
		return nil, err
	}
	limit := int64(2 * maxPayloadBytes)
	if resp.StatusCode >= http.StatusBadRequest {
		limit = 64 << 10
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: limit}
	return resp, nil
}

func (e *executor[Request, Response]) stream(ctx context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// Retry ownership belongs to the executor. Never replay a partially
	// consumed stream: neither usage nor the remote completion is known.
	stream := e.client.NewStreaming(ctx, params, option.WithMaxRetries(0), option.WithMiddleware(boundWire))
	defer stream.Close()
	events := 0
	for stream.Next() {
		events++
		if events > 32768 {
			return nil, errors.New("responses stream exceeds event limit")
		}
		event := stream.Current()
		switch event.Type {
		case "response.completed":
			if event.Response.Status != "completed" || event.Response.ID == "" {
				return nil, errors.New("responses completion has invalid identity or status")
			}
			if !event.Response.Usage.JSON.InputTokens.Valid() || !event.Response.Usage.JSON.OutputTokens.Valid() {
				return nil, errors.New("responses completion is missing token usage")
			}
			return &event.Response, nil
		case "response.failed", "response.incomplete", "error":
			return nil, errors.New("responses stream failed or ended incomplete")
		case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done",
			"response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done",
			"response.function_call_arguments.delta", "response.function_call_arguments.done", "response.refusal.delta", "response.refusal.done",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
			// Consume incremental events, but execute only the authoritative
			// completed response. No tool sees an incomplete argument string.
		default:
			return nil, errors.New("responses returned unsupported stream event")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := stream.Err(); err != nil {
		if api, ok := errors.AsType[*openai.Error](err); ok && events == 0 {
			return nil, &httpFailure{status: api.StatusCode}
		}
		return nil, errors.New("responses stream transport or decoding failure; usage may be unavailable")
	}
	return nil, errors.New("responses stream ended without a completed response; usage may be unavailable")
}
