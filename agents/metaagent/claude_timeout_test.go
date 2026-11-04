/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type stalledClaudeBody struct {
	ctx    context.Context
	prefix *strings.Reader
	closed bool
}

func (b *stalledClaudeBody) Read(p []byte) (int, error) {
	if n, err := b.prefix.Read(p); err != io.EOF {
		return n, err
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *stalledClaudeBody) Close() error {
	b.closed = true
	return nil
}

func TestClaudeRequestTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		callTimeout       time.Duration
		runTimeout        time.Duration
		streamStarted     bool
		wantParentExpired bool
	}{
		{name: "before headers", callTimeout: time.Second, runTimeout: 10 * time.Second},
		{name: "during stream", callTimeout: time.Second, runTimeout: 10 * time.Second, streamStarted: true},
		{name: "shorter parent", callTimeout: 10 * time.Second, runTimeout: time.Second, streamStarted: true, wantParentExpired: true},
		{name: "zero inherits parent", runTimeout: time.Second, streamStarted: true, wantParentExpired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				var body *stalledClaudeBody
				client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					defer r.Body.Close()
					calls++
					if !tc.streamStarted {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					body = &stalledClaudeBody{ctx: r.Context(), prefix: strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stall\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
				})}
				cfg := routedTestConfig(t)
				cfg.ClaudeRequestTimeout = tc.callTimeout
				agent, err := newClaudeAgentWithMessages[*testRequest](claudeAgentConstruction{
					messages:        anthropic.NewMessageService(option.WithoutEnvironmentDefaults(), option.WithBaseURL("https://claude.invalid"), option.WithAPIKey("test"), option.WithMaxRetries(0), option.WithHTTPClient(client)),
					providerModelID: "claude-sonnet-4-6",
				}, cfg)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), tc.runTimeout)
				defer cancel()
				_, err = agent.Execute(ctx, &testRequest{}, toolcall.EmptyTools{})
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Execute: got = %v, want deadline exceeded", err)
				}
				if workqueue.HasInfrastructureMarker(err) {
					t.Errorf("timeout marked as infrastructure: %v", err)
				}
				if got := ctx.Err() != nil; got != tc.wantParentExpired {
					t.Errorf("parent expired: got = %t, want = %t", got, tc.wantParentExpired)
				}
				if calls != 1 {
					t.Errorf("requests: got = %d, want = 1", calls)
				}
				if tc.streamStarted && (body == nil || body.prefix.Len() != 0 || !body.closed) {
					t.Error("stream must consume the prefix and close the body after cancellation")
				}
			})
		})
	}
}

func TestClaudeRequestTimeoutValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		model    string
		protocol modelrouter.Protocol
		timeout  time.Duration
	}{
		{name: "negative", model: "claude-sonnet-4-6", protocol: modelrouter.ProtocolAnthropicMessages, timeout: -time.Second},
		{name: "other backend", model: "gemini-2.5-flash", protocol: modelrouter.ProtocolGoogleGenAI, timeout: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := routedTestConfig(t)
			cfg.ClaudeRequestTimeout = tc.timeout
			if _, err := New[*testRequest](t.Context(), "project", "region", tc.model, cfg); err == nil {
				t.Fatal("New accepted invalid timeout configuration")
			}
			err := validateRoutedConfig(cfg)
			if err == nil {
				err = validateRoutedConfigForProtocol(tc.protocol, tc.model, cfg)
			}
			if err == nil {
				t.Fatal("routed validation accepted invalid timeout configuration")
			}
		})
	}
}
