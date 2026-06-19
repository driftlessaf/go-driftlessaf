/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"net/http"
	"time"

	"github.com/chainguard-dev/clog"
	httpmetrics "github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const (
	// EventType is the CloudEvent type for agent trace records.
	EventType = "dev.chainguard.driftlessaf.agent.trace.v1"

	ceRetryDelay  = 100 * time.Millisecond
	ceMaxRetry    = 3
	ceMaxInflight = 100
	ceSendTimeout = 30 * time.Second
)

// ceEmittingTracer wraps an inner Tracer and emits a CloudEvent for every
// completed trace. Sends are non-blocking (bounded errgroup) so emission
// does not delay the reconciler. Call Drain to flush in-flight events
// before process exit.
type ceEmittingTracer[T any] struct {
	inner  Tracer[T]
	client cloudevents.Client
	source string
	eg     errgroup.Group
}

// WithCloudEventEmission wraps inner so that each call to RecordTrace also
// emits the trace as a CloudEvent. The caller provides a pre-built
// cloudevents.Client (see NewBrokerClient) and a source identifier
// (e.g. the OCTO_IDENTITY of the reconciler). The CloudEvent type is
// always EventType.
//
// Call Drain on the returned tracer (via type assertion) before process
// exit to flush in-flight events.
func WithCloudEventEmission[T any](inner Tracer[T], client cloudevents.Client, source string) Tracer[T] {
	t := &ceEmittingTracer[T]{
		inner:  inner,
		client: client,
		source: source,
	}
	t.eg.SetLimit(ceMaxInflight)
	return t
}

func (t *ceEmittingTracer[T]) NewTrace(ctx context.Context, prompt string, opts ...StartTraceOption) *Trace[T] {
	trace := t.inner.NewTrace(ctx, prompt, opts...)
	// Wire the per-span emitter so LLMTurn.End ships a per-turn CloudEvent
	// for any turn that recorded a payload. Gating already happens in
	// RecordRequest/RecordResponse — if payloads were never recorded, the
	// emitter is invoked with nothing and short-circuits.
	trace.spanEmitter = t.emitSpan
	return trace
}

// emitSpan sends a per-turn CloudEvent through the same broker as the
// per-trace event. Uses the same bounded errgroup as RecordTrace; the
// caller's End() returns immediately under normal load but blocks on
// eg.Go once the in-flight cap (ceMaxInflight) is reached. Multi-turn
// traces multiply pressure on the cap relative to the per-trace-only
// design.
func (t *ceEmittingTracer[T]) emitSpan(ctx context.Context, span RecordedSpan) error {
	ce := cloudevents.NewEvent()
	ce.SetID(span.SpanID)
	ce.SetType(SpanEventType)
	ce.SetSource(t.source)
	ce.SetSubject(span.TraceID)
	ce.SetTime(span.RecordedAt)

	if err := ce.SetData(cloudevents.ApplicationJSON, span); err != nil {
		clog.ErrorContext(ctx, "Failed to set span CloudEvent data",
			"trace_id", span.TraceID,
			"span_id", span.SpanID,
			"error", err,
		)
		return err
	}

	t.eg.Go(func() error {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ceSendTimeout)
		defer cancel()

		rctx := cloudevents.ContextWithRetriesExponentialBackoff(sendCtx, ceRetryDelay, ceMaxRetry)
		if result := t.client.Send(rctx, ce); cloudevents.IsUndelivered(result) || cloudevents.IsNACK(result) {
			clog.ErrorContext(ctx, "Failed to deliver agent span event",
				"trace_id", span.TraceID,
				"span_id", span.SpanID,
				"error", result,
			)
		}
		return nil
	})
	return nil
}

func (t *ceEmittingTracer[T]) RecordTrace(trace *Trace[T]) {
	// Delegate to the inner tracer first (logging, evals, etc.).
	t.inner.RecordTrace(trace)

	ctx := trace.ctx

	ce := cloudevents.NewEvent()
	ce.SetID(trace.ID)
	ce.SetType(EventType)
	ce.SetSource(t.source)
	ce.SetSubject(trace.ExecContext.ReconcilerKey)
	ce.SetTime(trace.StartTime)

	if err := ce.SetData(cloudevents.ApplicationJSON, trace); err != nil {
		clog.ErrorContext(ctx, "Failed to set CloudEvent data",
			"trace_id", trace.ID,
			"error", err,
		)
		return
	}

	t.eg.Go(func() error {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ceSendTimeout)
		defer cancel()

		rctx := cloudevents.ContextWithRetriesExponentialBackoff(sendCtx, ceRetryDelay, ceMaxRetry)
		if result := t.client.Send(rctx, ce); cloudevents.IsUndelivered(result) || cloudevents.IsNACK(result) {
			clog.ErrorContext(ctx, "Failed to deliver agent trace event",
				"trace_id", trace.ID,
				"error", result,
			)
		}
		return nil
	})
}

// Drain flushes all in-flight CloudEvent sends. Call before process exit.
func (t *ceEmittingTracer[T]) Drain() {
	_ = t.eg.Wait()
}

// NewBrokerClient creates a CloudEvents HTTP client authenticated with
// an ID token for the given broker URL. Call this once at startup and
// pass the client to WithCloudEventEmission or middleware that wraps it.
//
// If brokerURL is empty or client construction fails, NewBrokerClient
// returns nil with a warning log. Callers should treat a nil client as
// "emission disabled" and skip wrapping the tracer.
//
// The ID token is signed directly from the ambient ADC (the common case: a
// reconciler running as its own service account). When the identity that
// authenticates the process differs from the identity authorized to call the
// broker — and especially when the ambient credential is a federated
// (external_account) one that cannot mint an ID token directly — use
// NewBrokerClientImpersonating instead.
//
// opts are forwarded to idtoken.NewTokenSource.
func NewBrokerClient(ctx context.Context, brokerURL string, opts ...option.ClientOption) cloudevents.Client {
	if brokerURL == "" {
		return nil
	}

	tokenSource, err := idtoken.NewTokenSource(ctx, brokerURL, opts...)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to create ID token source for trace events, disabling: %v", err)
		return nil
	}

	return brokerClientFromTokenSource(ctx, brokerURL, tokenSource)
}

// NewBrokerClientImpersonating is like NewBrokerClient but mints the broker ID
// token by impersonating targetPrincipal (a service account email) rather than
// signing directly with the ambient ADC. The process's ADC must hold
// roles/iam.serviceAccountTokenCreator on targetPrincipal.
//
// This is the path for a producer whose ambient credential is a federated
// (external_account) WIF credential: idtoken cannot mint an ID token directly
// from such a credential, but the ADC can obtain an access token and use it to
// impersonate targetPrincipal, which generates the ID token. The returned
// source refreshes automatically. brokerURL empty or source construction
// failure returns nil (emission disabled), same as NewBrokerClient.
func NewBrokerClientImpersonating(ctx context.Context, brokerURL, targetPrincipal string) cloudevents.Client {
	if brokerURL == "" {
		return nil
	}

	tokenSource, err := impersonate.IDTokenSource(ctx, impersonate.IDTokenConfig{
		Audience:        brokerURL,
		TargetPrincipal: targetPrincipal,
		IncludeEmail:    true,
	})
	if err != nil {
		clog.WarnContextf(ctx, "Failed to create impersonated ID token source for trace events, disabling: %v", err)
		return nil
	}

	return brokerClientFromTokenSource(ctx, brokerURL, tokenSource)
}

// brokerClientFromTokenSource builds the CloudEvents HTTP client that targets
// brokerURL and authenticates every request with tokenSource. Returns nil with
// a warning log if client construction fails.
func brokerClientFromTokenSource(ctx context.Context, brokerURL string, tokenSource oauth2.TokenSource) cloudevents.Client {
	innerTransport := httpmetrics.ExtractInnerTransport(http.DefaultTransport)
	var baseTransport *http.Transport
	if t, ok := innerTransport.(*http.Transport); ok {
		baseTransport = t.Clone()
	} else {
		baseTransport = &http.Transport{}
	}

	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(brokerURL),
		cehttp.WithClient(http.Client{Transport: httpmetrics.WrapTransport(&oauth2.Transport{
			Source: tokenSource,
			Base:   baseTransport,
		})}),
	)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to create CloudEvents client for trace events, disabling: %v", err)
		return nil
	}

	return client
}
