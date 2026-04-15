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
	inner     Tracer[T]
	client    cloudevents.Client
	eventType string
	source    string
	eg        errgroup.Group
}

// WithCloudEventEmission wraps inner so that each call to RecordTrace also
// emits the trace as a CloudEvent. The caller provides a pre-built
// cloudevents.Client (see NewBrokerClient), the CloudEvent type string,
// and a source identifier (e.g. "github-pr-autofix-reconciler").
//
// Call Drain on the returned tracer (via type assertion) before process
// exit to flush in-flight events.
func WithCloudEventEmission[T any](inner Tracer[T], client cloudevents.Client, eventType, source string) Tracer[T] {
	t := &ceEmittingTracer[T]{
		inner:     inner,
		client:    client,
		eventType: eventType,
		source:    source,
	}
	t.eg.SetLimit(ceMaxInflight)
	return t
}

func (t *ceEmittingTracer[T]) NewTrace(ctx context.Context, prompt string) *Trace[T] {
	return t.inner.NewTrace(ctx, prompt)
}

func (t *ceEmittingTracer[T]) RecordTrace(trace *Trace[T]) {
	// Delegate to the inner tracer first (logging, evals, etc.).
	t.inner.RecordTrace(trace)

	ctx := trace.ctx

	ce := cloudevents.NewEvent()
	ce.SetID(trace.ID)
	ce.SetType(t.eventType)
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
func NewBrokerClient(ctx context.Context, brokerURL string) cloudevents.Client {
	if brokerURL == "" {
		return nil
	}

	innerTransport := httpmetrics.ExtractInnerTransport(http.DefaultTransport)
	var baseTransport *http.Transport
	if t, ok := innerTransport.(*http.Transport); ok {
		baseTransport = t.Clone()
	} else {
		baseTransport = &http.Transport{}
	}

	tokenSource, err := idtoken.NewTokenSource(ctx, brokerURL)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to create ID token source for trace events, disabling: %v", err)
		return nil
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
