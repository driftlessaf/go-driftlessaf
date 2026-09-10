/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/openai/openai-go"
)

func TestHTTPFailureDiagnostics(t *testing.T) {
	t.Parallel()
	requestID := uuid.NewString()
	openaiID := "req_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	for _, tt := range []struct {
		name, body, errorType, header, id, code, wantID string
	}{
		{name: "AWS body", body: `{"__type":"AccessDeniedException","message":"secret-canary"}`, header: "X-Amzn-Requestid", id: requestID, code: "AccessDeniedException", wantID: requestID},
		{name: "AWS header", body: `{"message":"secret-canary"}`, errorType: "AccessDeniedException:secret-canary", header: "X-Amzn-Requestid", id: requestID, code: "AccessDeniedException", wantID: requestID},
		{name: "OpenAI code", body: `{"error":{"code":"permission_denied","type":"invalid_request_error","message":"secret-canary","param":"secret-canary"}}`, header: "X-Request-Id", id: openaiID, code: "permission_denied", wantID: openaiID},
		{name: "OpenAI type", body: `{"error":{"type":"authentication_error","message":"secret-canary"}}`, code: "authentication_error"},
		{name: "unknown details", body: `{"error":{"code":"secret-canary","type":"secret-canary","message":"secret-canary"}}`, errorType: "secret-canary", header: "X-Request-Id", id: "secret-canary"},
		{name: "unknown code keeps correlation", body: `{"error":{"code":"secret-canary"}}`, header: "X-Amzn-Requestid", id: requestID, wantID: requestID},
		{name: "no details", body: `{}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requests := 0
			e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Amzn-Errortype", tt.errorType)
				w.Header().Set("Authorization", "Bearer secret-canary")
				w.Header().Set("Set-Cookie", "secret-canary")
				if tt.header != "" {
					w.Header().Set(tt.header, tt.id)
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, tt.body)
			}, config(t))
			_, err := e.Execute(t.Context(), request{}, nil)
			failure, ok := errors.AsType[*httpFailure](err)
			if !ok {
				t.Fatalf("error: got %v, want httpFailure", err)
			}
			if failure.code != tt.code || failure.requestID != tt.wantID {
				t.Fatalf("diagnostics: got code=%q requestID=%q, want code=%q requestID=%q", failure.code, failure.requestID, tt.code, tt.wantID)
			}
			if got := statusCode(err); got != http.StatusForbidden || retryable(err) || requests != 1 {
				t.Fatalf("request: got status=%d retryable=%t attempts=%d, want 403 false 1", got, retryable(err), requests)
			}
			if _, ok := errors.AsType[*openai.Error](err); ok {
				t.Fatal("error chain: got raw SDK error, want sanitized failure only")
			}
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err)} {
				if strings.Contains(rendered, "secret-canary") || strings.Contains(rendered, "http://") {
					t.Fatalf("error leaked provider details: %q", rendered)
				}
				if tt.code != "" && !strings.Contains(rendered, "code="+tt.code) || tt.wantID != "" && !strings.Contains(rendered, "request_id="+tt.wantID) {
					t.Fatalf("error omitted safe diagnostics: %q", rendered)
				}
			}
		})
	}
}

func TestHTTPFailureHints(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		codes    []string
		wantHint string
	}{
		{
			codes:    []string{"AccessDeniedException", "permission_denied"},
			wantHint: "; access denied: verify IAM, model access, and retention eligibility",
		},
		{
			codes:    []string{"ExpiredTokenException", "InvalidSignatureException", "UnrecognizedClientException", "invalid_api_key", "authentication_error"},
			wantHint: "; authentication rejected: verify credentials and request signing",
		},
		{
			codes:    []string{"model_not_found", "ResourceNotFoundException"},
			wantHint: "; verify the model identifier and regional availability",
		},
		{
			codes:    []string{"insufficient_quota", "ServiceQuotaExceededException"},
			wantHint: "; verify provider quota",
		},
		{
			codes: []string{"unknown_code", "ValidationException", "invalid_request_error"},
		},
	} {
		for _, code := range tt.codes {
			t.Run(code, func(t *testing.T) {
				t.Parallel()
				requestID := uuid.NewString()
				want := fmt.Sprintf("responses request failed (HTTP 403); code=%s; request_id=%s%s", code, requestID, tt.wantHint)
				if got := (&httpFailure{status: http.StatusForbidden, code: code, requestID: requestID}).Error(); got != want {
					t.Errorf("Error(): got %q, want %q", got, want)
				}
			})
		}
	}
	t.Run("status only", func(t *testing.T) {
		t.Parallel()
		if got, want := (&httpFailure{status: http.StatusForbidden}).Error(), "responses request failed (HTTP 403)"; got != want {
			t.Errorf("Error(): got %q, want %q", got, want)
		}
	})
}

func TestSafeRequestID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "secret-canary", strings.Repeat("a", 1024), strings.Repeat("g", 36), "req_" + strings.Repeat("z", 32), "req_" + strings.Repeat("a", 31) + "\n", strings.Repeat("a", 35) + "\x1b", "https://example.com/secret", strings.Repeat("a", 36)} {
		if got := safeRequestID(id); got != "" {
			t.Errorf("safeRequestID(%q): got %q, want empty", id, got)
		}
	}
}
