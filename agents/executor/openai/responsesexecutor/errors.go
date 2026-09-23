/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
)

// httpFailure retains only allowlisted error categories and a narrowly validated
// correlation ID. Never wrap the SDK error: it retains bodies and credentials.
type httpFailure struct {
	status    int
	code      string
	requestID string
}

func (e *httpFailure) Error() string {
	message := fmt.Sprintf("responses request failed (HTTP %d)", e.status)
	if e.code != "" {
		message += "; code=" + e.code
	}
	if e.requestID != "" {
		message += "; request_id=" + e.requestID
	}
	switch e.code {
	case "AccessDeniedException", "permission_denied":
		message += "; access denied: verify IAM, model access, and retention eligibility"
	case "ExpiredTokenException", "InvalidSignatureException", "UnrecognizedClientException", "invalid_api_key", "authentication_error":
		message += "; authentication rejected: verify credentials and request signing"
	case "model_not_found", "ResourceNotFoundException":
		message += "; verify the model identifier and regional availability"
	case "insufficient_quota", "ServiceQuotaExceededException":
		message += "; verify provider quota"
	}
	return message
}

func safeHTTPFailure(api *openai.Error) *httpFailure {
	failure := &httpFailure{status: api.StatusCode}
	// AWS errors can use a top-level __type instead of the OpenAI error envelope.
	// The SDK's RawJSON contains only the nested error object. Its response body
	// retains the full bounded payload, including AWS's top-level __type.
	var aws struct {
		Type string `json:"__type"`
	}
	if api.Response != nil {
		if api.Response.Body != nil {
			defer api.Response.Body.Close()
			if raw, err := io.ReadAll(io.LimitReader(api.Response.Body, (64<<10)+1)); err == nil && len(raw) <= 64<<10 {
				if err := json.Unmarshal(raw, &aws); err != nil {
					aws.Type = ""
				}
			}
		}
		failure.requestID = safeHeaderRequestID(api.Response.Header)
	}
	candidates := []string{api.Code, aws.Type}
	if api.Response != nil {
		candidates = append(candidates, api.Response.Header.Get("X-Amzn-Errortype"))
	}
	candidates = append(candidates, api.Type)
	for _, code := range candidates {
		// AWS may suffix the error type with a colon-delimited detail. Ignore
		// that untrusted detail; emit only exact matches from this closed list.
		code, _, _ = strings.Cut(code, ":")
		switch code {
		case "AccessDeniedException", "ExpiredTokenException", "InvalidSignatureException", "UnrecognizedClientException",
			"ValidationException", "ResourceNotFoundException", "ThrottlingException", "ServiceQuotaExceededException",
			"ModelNotReadyException", "ModelTimeoutException", "InternalServerException", "ServiceUnavailableException",
			"invalid_api_key", "authentication_error", "permission_denied", "model_not_found", "insufficient_quota",
			"rate_limit_exceeded", "invalid_request_error", "server_error":
			failure.code = code
			return failure
		}
	}
	return failure
}

func safeHeaderRequestID(header http.Header) string {
	return cmp.Or(safeRequestID(header.Get("X-Amzn-Requestid")), safeRequestID(header.Get("X-Request-Id")))
}

// streamFailure deliberately carries no HTTP status or underlying SDK error:
// terminal events must never replay a potentially billable partial stream.
type streamFailure struct {
	eventType  string
	code       string
	reason     string
	requestID  string
	responseID string
}

func (e *streamFailure) Error() string {
	message := "responses stream failed or ended incomplete; event=" + e.eventType
	if e.code != "" {
		message += "; code=" + e.code
	}
	if e.reason != "" {
		message += "; incomplete_reason=" + e.reason
	}
	if e.requestID != "" {
		message += "; request_id=" + e.requestID
	}
	if e.responseID != "" {
		message += "; response_id=" + e.responseID
	}
	return message + "; usage may be unavailable"
}

// Called only for the three recognized terminal event types. Retain no raw
// event, provider message, output, or credentials in the returned error.
func safeStreamFailure(event responses.ResponseStreamEventUnion, response *http.Response, responseID string) *streamFailure {
	failure := &streamFailure{eventType: event.Type, responseID: responseID}
	if response != nil {
		failure.requestID = safeHeaderRequestID(response.Header)
	}
	code := string(event.Response.Error.Code)
	if event.Type == "error" {
		code = event.Code
	}
	switch code {
	case "server_error", "rate_limit_exceeded", "invalid_prompt", "vector_store_timeout",
		"invalid_image", "invalid_image_format", "invalid_base64_image", "invalid_image_url",
		"image_too_large", "image_too_small", "image_parse_error", "image_content_policy_violation",
		"invalid_image_mode", "image_file_too_large", "unsupported_image_media_type", "empty_image_file",
		"failed_to_download_image", "image_file_not_found", "invalid_request_error":
		failure.code = code
	}
	if event.Type == "response.incomplete" {
		switch reason := event.Response.IncompleteDetails.Reason; reason {
		case "max_output_tokens", "max_tokens", "content_filter":
			failure.reason = reason
		}
	}
	return failure
}

// The pinned SDK's ssestream.Stream.Next intercepts SSE data containing a
// top-level "error" field and returns a generic error before our executor
// receives the event. The error message has the fixed prefix below, followed
// by the raw JSON value of the "error" field. Parse only the allowlisted code
// from that JSON; never retain the message, param, or any other field.
const sdkInterceptedPrefix = "received error while streaming: "

// safeInterceptedStreamError detects the SDK's intercepted top-level error
// envelope and returns a streamFailure with only the allowlisted code and
// validated correlation IDs. It returns nil when the error is not from the
// intercepted path, so the caller can fall through to the generic message.
func safeInterceptedStreamError(err error, response *http.Response, responseID string) *streamFailure {
	suffix, ok := strings.CutPrefix(err.Error(), sdkInterceptedPrefix)
	if !ok {
		return nil
	}
	failure := &streamFailure{
		eventType:  "error",
		responseID: responseID,
	}
	if response != nil {
		failure.requestID = safeHeaderRequestID(response.Header)
	}
	// The suffix is the gjson string representation of the "error" field.
	// Parse only the "code" and "type" sub-fields; ignore message and param.
	var envelope struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(suffix), &envelope); err == nil {
		for _, code := range []string{envelope.Code, envelope.Type} {
			switch code {
			case "server_error", "rate_limit_exceeded", "invalid_prompt", "vector_store_timeout",
				"invalid_image", "invalid_image_format", "invalid_base64_image", "invalid_image_url",
				"image_too_large", "image_too_small", "image_parse_error", "image_content_policy_violation",
				"invalid_image_mode", "image_file_too_large", "unsupported_image_media_type", "empty_image_file",
				"failed_to_download_image", "image_file_not_found", "invalid_request_error":
				failure.code = code
				return failure
			}
		}
	}
	return failure
}

// safeBedrockStreamFailure converts a SafeBedrockError into a streamFailure
// carrying only the allowlisted category and validated correlation IDs. It
// never retains the original error or any raw SDK content.
func safeBedrockStreamFailure(be bedrockruntime.SafeBedrockError, response *http.Response, responseID string) *streamFailure {
	failure := &streamFailure{
		eventType:  "bedrock",
		responseID: responseID,
	}
	if response != nil {
		failure.requestID = safeHeaderRequestID(response.Header)
	}
	// Prefer the validated service request ID from the Bedrock error over the
	// HTTP response header when both are present.
	if id := be.BedrockRequestID(); id != "" {
		failure.requestID = id
	}
	if code := be.BedrockCategory(); code != "" {
		failure.code = code
	}
	return failure
}

// Accept only UUIDs or resp_ followed by a known hexadecimal ID length.
// Other formats are omitted rather than risking an echoed prompt or secret.
func safeResponseID(id string) string {
	if value, ok := strings.CutPrefix(id, "resp_"); ok {
		switch len(value) {
		case 32, 48, 64:
		default:
			return ""
		}
		for _, c := range value {
			switch {
			case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
			default:
				return ""
			}
		}
		return id
	}
	if strings.HasPrefix(id, "req_") {
		return ""
	}
	return safeRequestID(id)
}

// Accept canonical UUIDs (AWS) and req_ followed by 32 hexadecimal digits.
// Do not accept arbitrary printable strings: they can contain echoed secrets.
func safeRequestID(id string) string {
	if len(id) != 36 {
		return ""
	}
	value, prefixed := strings.CutPrefix(id, "req_")
	for i, c := range value {
		if !prefixed && (i == 8 || i == 13 || i == 18 || i == 23) {
			if c != '-' {
				return ""
			}
			continue
		}
		switch {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return ""
		}
	}
	return id
}
