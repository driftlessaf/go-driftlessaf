/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"net/http"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/openaiauth"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

// NewOpenAIDirectResponsesAdapter constructs a native Responses adapter for
// OpenAI's first-party API, authenticated by workload identity federation
// (see openaiauth). There is no API-key path: ambient OPENAI_* settings are
// never read. Application Default Credentials are discovered on the first
// request, not here.
//
// Every binding from one adapter shares one token exchange and its cached
// bearer, so agents built per run do not each exchange a new token.
func NewOpenAIDirectResponsesAdapter(cfg openaiauth.Config) (OpenAIResponsesAdapter, error) {
	return newOpenAIDirectResponsesAdapter(cfg, http.DefaultTransport)
}

// newOpenAIDirectResponsesAdapter takes the transport that carries both the
// token exchange and API requests, so tests can route OpenAI's fixed hosts to
// a local server.
func newOpenAIDirectResponsesAdapter(cfg openaiauth.Config, transport http.RoundTripper) (OpenAIResponsesAdapter, error) {
	identity, err := cfg.WorkloadIdentity()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAdapter, err)
	}
	if transport == nil {
		return nil, fmt.Errorf("%w: OpenAI transport is nil", ErrInvalidAdapter)
	}
	// The SDK runs a foreground token exchange (cold cache or an expired
	// bearer) on the leading request's context and gives its result to every
	// concurrent waiter. If that request is cancelled, the waiters fail with
	// it. This is accepted: it happens only on cold start or after an idle
	// stretch, and costs each waiter one retried request.
	workloadIdentity := option.WithWorkloadIdentity(identity)
	return func(_ context.Context, plan modelrouter.Plan) (OpenAIResponsesBinding, error) {
		if err := validateProviderPlan(plan, modelrouter.ProtocolOpenAIResponses,
			modelrouter.ProviderOpenAI, agenttrace.SystemOpenAI, agenttrace.SystemOpenAI); err != nil {
			return OpenAIResponsesBinding{}, err
		}
		// Service-only construction skips NewClient's ambient OPENAI_* defaults.
		return NewOpenAIResponsesBinding(plan, responses.NewResponseService(
			option.WithBaseURL(cfg.APIBaseURL()),
			option.WithHTTPClient(&http.Client{
				Transport: transport,
				// The bearer must reach only the API host.
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}),
			workloadIdentity,
			option.WithMaxRetries(0),
			option.WithJSONSet("store", false),
		), nil)
	}, nil
}
