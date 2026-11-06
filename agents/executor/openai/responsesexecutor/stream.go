/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
	"chainguard.dev/driftlessaf/agents/internal/calltiming"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

const maxPayloadBytes = 8 << 20

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

// knownStreamEvents are the non-terminal event types the pinned openai-go
// decodes in ResponseStreamEventUnion. stream consumes these silently and logs
// any other type, so add the types a newer openai-go pin decodes.
var knownStreamEvents = map[string]struct{}{
	"response.audio.delta":                         {},
	"response.audio.done":                          {},
	"response.audio.transcript.delta":              {},
	"response.audio.transcript.done":               {},
	"response.code_interpreter_call.completed":     {},
	"response.code_interpreter_call.in_progress":   {},
	"response.code_interpreter_call.interpreting":  {},
	"response.code_interpreter_call_code.delta":    {},
	"response.code_interpreter_call_code.done":     {},
	"response.compaction.compacting":               {},
	"response.content_part.added":                  {},
	"response.content_part.done":                   {},
	"response.created":                             {},
	"response.custom_tool_call_input.delta":        {},
	"response.custom_tool_call_input.done":         {},
	"response.file_search_call.completed":          {},
	"response.file_search_call.in_progress":        {},
	"response.file_search_call.searching":          {},
	"response.function_call_arguments.delta":       {},
	"response.function_call_arguments.done":        {},
	"response.image_generation_call.completed":     {},
	"response.image_generation_call.generating":    {},
	"response.image_generation_call.in_progress":   {},
	"response.image_generation_call.partial_image": {},
	"response.in_progress":                         {},
	"response.mcp_call.completed":                  {},
	"response.mcp_call.failed":                     {},
	"response.mcp_call.in_progress":                {},
	"response.mcp_call_arguments.delta":            {},
	"response.mcp_call_arguments.done":             {},
	"response.mcp_list_tools.completed":            {},
	"response.mcp_list_tools.failed":               {},
	"response.mcp_list_tools.in_progress":          {},
	"response.output_item.added":                   {},
	"response.output_item.done":                    {},
	"response.output_text.annotation.added":        {},
	"response.output_text.delta":                   {},
	"response.output_text.done":                    {},
	"response.queued":                              {},
	"response.reasoning_summary_part.added":        {},
	"response.reasoning_summary_part.done":         {},
	"response.reasoning_summary_text.delta":        {},
	"response.reasoning_summary_text.done":         {},
	"response.reasoning_text.delta":                {},
	"response.reasoning_text.done":                 {},
	"response.refusal.delta":                       {},
	"response.refusal.done":                        {},
	"response.shell_call_command.added":            {},
	"response.shell_call_command.delta":            {},
	"response.shell_call_command.done":             {},
	"response.shell_call_output_content.delta":     {},
	"response.shell_call_output_content.done":      {},
	"response.web_search_call.completed":           {},
	"response.web_search_call.in_progress":         {},
	"response.web_search_call.searching":           {},
}

// maxLoggedUnknownEvents bounds the distinct unknown types logged per stream,
// so a provider that varies the type on every event cannot flood the log.
const maxLoggedUnknownEvents = 8

// logUnknownStreamEvent logs typ the first time it appears in a stream.
// seen holds the types already logged for that stream.
func logUnknownStreamEvent(ctx context.Context, seen map[string]struct{}, typ string) {
	if _, ok := seen[typ]; ok || len(seen) >= maxLoggedUnknownEvents {
		return
	}
	seen[typ] = struct{}{}
	if len(typ) > 128 {
		typ = typ[:128]
	}
	clog.WarnContext(ctx, "Consuming unknown Responses stream event type", "type", typ)
}

// stream sends one Responses request and returns the completed response.
// timer measures the request and marks the first streamed delta.
func (e *executor[Request, Response]) stream(ctx context.Context, params responses.ResponseNewParams, timer *calltiming.Timer) (*responses.Response, error) {
	if e.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.config.RequestTimeout)
		defer cancel()
	}
	// Retry ownership belongs to the executor. Never replay a partially
	// consumed stream: neither usage nor the remote completion is known.
	var httpResponse *http.Response
	stream := e.client.NewStreaming(timer.Context(ctx), params, option.WithMaxRetries(0), option.WithMiddleware(boundWire), option.WithResponseInto(&httpResponse))
	defer stream.Close()
	events := 0
	unknown := make(map[string]struct{}, maxLoggedUnknownEvents)
	responseID := ""
	for stream.Next() {
		events++
		if events > 32768 {
			return nil, errors.New("responses stream exceeds event limit")
		}
		event := stream.Current()
		responseID = cmp.Or(safeResponseID(event.Response.ID), responseID)
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
			return nil, safeStreamFailure(event, httpResponse, responseID)
		default:
			// Every other event is incremental: consume it, but execute only
			// the authoritative completed response, so no tool sees an
			// incomplete argument string. Providers add event types without
			// notice, so an unknown type is logged, not failed. The first
			// delta of any kind is the first output token.
			if strings.HasSuffix(event.Type, ".delta") {
				timer.MarkFirstToken()
			}
			if _, known := knownStreamEvents[event.Type]; !known {
				logUnknownStreamEvent(ctx, unknown, event.Type)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := stream.Err(); err != nil {
		if api, ok := errors.AsType[*openai.Error](err); ok && events == 0 {
			return nil, safeHTTPFailure(api)
		}
		if f := safeInterceptedStreamError(err, httpResponse, responseID); f != nil {
			return nil, f
		}
		if be, ok := errors.AsType[bedrockruntime.SafeBedrockError](err); ok {
			return nil, safeBedrockStreamFailure(be, httpResponse, responseID)
		}
		return nil, errors.New("responses stream transport or decoding failure; usage may be unavailable")
	}
	return nil, errors.New("responses stream ended without a completed response; usage may be unavailable")
}
