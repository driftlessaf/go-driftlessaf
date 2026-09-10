/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/openai/openai-go"
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
		for _, header := range []string{"X-Amzn-Requestid", "X-Request-Id"} {
			if id := safeRequestID(api.Response.Header.Get(header)); id != "" {
				failure.requestID = id
				break
			}
		}
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
