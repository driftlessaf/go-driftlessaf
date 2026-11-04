/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type timeoutTransport func(*http.Request) (*http.Response, error)

func (f timeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	defer r.Body.Close()
	return f(r)
}

func TestRequestTimeoutRenewsOnRetry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var deadlines []time.Time
		client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL("https://claude.invalid"), option.WithAPIKey("test"), option.WithMaxRetries(0),
			option.WithHTTPClient(&http.Client{Transport: timeoutTransport(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Error("request has no deadline")
				}
				deadlines = append(deadlines, deadline)
				if len(deadlines) == 1 {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)), Request: r}, nil
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}))
		exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, promptbuilder.MustNewPrompt("hello"),
			claudeexecutor.WithRequestTimeout[errCapRequest, errCapResponse](time.Second),
			claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(2)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if _, err := exec.Execute(ctx, errCapRequest{}, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Execute: got = %v, want deadline exceeded", err)
		}
		if ctx.Err() != nil {
			t.Fatalf("parent expired: %v", ctx.Err())
		}
		if len(deadlines) != 2 {
			t.Fatalf("requests: got = %d, want = 2", len(deadlines))
		}
		if !deadlines[1].After(deadlines[0]) {
			t.Errorf("retry deadline = %v, want later than %v", deadlines[1], deadlines[0])
		}
	})
}

func TestRequestTimeoutBoundsTokenCounting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL("https://claude.invalid"), option.WithAPIKey("test"), option.WithMaxRetries(0),
			option.WithHTTPClient(&http.Client{Transport: timeoutTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/v1/messages/count_tokens" {
					t.Errorf("path: got = %q, want count_tokens", r.URL.Path)
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}))
		exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, boundPrompt(t, strings.Repeat("x", 70000)),
			claudeexecutor.WithRequestTimeout[errCapRequest, errCapResponse](time.Second),
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](65536),
			claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(2)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if _, err := exec.Execute(ctx, errCapRequest{}, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Execute: got = %v, want deadline exceeded", err)
		}
		if ctx.Err() != nil {
			t.Fatalf("parent expired: %v", ctx.Err())
		}
		if calls != 1 {
			t.Errorf("requests: got = %d, want = 1", calls)
		}
	})
}

func TestRequestTimeoutDoesNotBoundTools(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var requestContexts []context.Context
		client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL("https://claude.invalid"), option.WithAPIKey("test"), option.WithMaxRetries(0),
			option.WithHTTPClient(&http.Client{Transport: timeoutTransport(func(r *http.Request) (*http.Response, error) {
				requestContexts = append(requestContexts, r.Context())
				if err := r.Context().Err(); err != nil {
					t.Errorf("request started with canceled context: %v", err)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				assertRequestValid(t, len(requestContexts), body)
				turn := latencyFinalTurn
				if len(requestContexts) == 1 {
					turn = latencyToolCallTurn
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(sseBody(t, turn))), Request: r}, nil
			})}))
		exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, promptbuilder.MustNewPrompt("hello"),
			claudeexecutor.WithRequestTimeout[errCapRequest, errCapResponse](time.Second),
			claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		wantDeadline, _ := ctx.Deadline()
		called := false
		tools := map[string]claudetool.Metadata[errCapResponse]{
			"lookup": {
				Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Type: "object"}},
				Handler: func(ctx context.Context, _ anthropic.ToolUseBlock, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) map[string]any {
					called = true
					if deadline, _ := ctx.Deadline(); !deadline.Equal(wantDeadline) {
						t.Errorf("tool deadline: got = %v, want = %v", deadline, wantDeadline)
					}
					if len(requestContexts) != 1 || requestContexts[0].Err() != context.Canceled {
						t.Error("first request context must be canceled before tool execution")
					}
					time.Sleep(2 * time.Second) // Virtual time exceeds the request budget while the tool runs.
					if err := ctx.Err(); err != nil {
						t.Errorf("tool canceled: %v", err)
					}
					return map[string]any{"ok": true}
				},
			},
		}
		got, err := exec.Execute(ctx, errCapRequest{}, tools)
		if err != nil || got.Answer != "42" || !called {
			t.Fatalf("Execute: got = %+v, %v, tool called = %t; want answer 42 after tool execution", got, err, called)
		}
		if len(requestContexts) != 2 {
			t.Fatalf("request contexts: got = %d, want = 2", len(requestContexts))
		}
		for i, ctx := range requestContexts {
			if err := ctx.Err(); err != context.Canceled {
				t.Errorf("request %d context: got = %v, want canceled", i, err)
			}
		}
	})
}
