/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace/payloadcrypt"
	"github.com/chainguard-dev/clog"
	httpmetrics "github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const (
	// EventType is the CloudEvent type for agent trace records.
	EventType = "dev.chainguard.driftlessaf.agent.trace.v1"

	ceRetryDelay                  = 100 * time.Millisecond
	ceMaxRetry                    = 3
	ceMaxInflight                 = 100
	ceSendTimeout                 = 30 * time.Second
	ceSealTimeout                 = 30 * time.Second
	maxTraceEventDataBytes        = 8 * 1024 * 1024
	maxTracePayloadFieldBytes     = 64 * 1024
	payloadTruncatedMetadataField = "driftlessaf.payload.truncated"
	cloudEventMeterName           = "chainguard.ai.agents.agenttrace"
)

var errTraceMarshal = errors.New("trace marshal failed")

// newEmissionCounter creates the emission counter from the meter provider
// that is installed when the emitter is constructed. httpmetrics installs a
// no-op global provider at init, so an instrument created during package
// initialisation would stay a no-op after the service installs its SDK
// provider.
func newEmissionCounter() metric.Int64Counter {
	counter, err := otel.Meter(
		cloudEventMeterName,
		metric.WithInstrumentationVersion("1.0.0"),
	).Int64Counter(
		"agenttrace.cloudevent.emissions",
		metric.WithDescription("Agent trace CloudEvent emission attempts by event type and outcome"),
		metric.WithUnit("{events}"),
	)
	if err != nil {
		clog.WarnContext(context.Background(), "Failed to create agent trace emission counter", "error", err)
		return noop.Int64Counter{}
	}

	return counter
}

// ceEmittingTracer wraps an inner Tracer and emits a CloudEvent for every
// completed trace. Sends are non-blocking (bounded errgroup) so emission
// does not delay the reconciler. Call Drain to flush in-flight events
// before process exit.
type ceEmittingTracer[T any] struct {
	inner              Tracer[T]
	client             cloudevents.Client
	source             string
	emissions          metric.Int64Counter
	boundTracePayloads bool
	// enc, when non-nil, seals the sensitive free-text payload fields of every
	// emitted trace/span event before it leaves the process. Nil leaves enabled
	// payloads in plaintext. Whether either form is present in the event is
	// controlled by WithEmitPayloads if set on ctx, else by WithPayloadsEnabled.
	enc *payloadcrypt.Encryptor
	eg  errgroup.Group
}

// CEOption configures a ceEmittingTracer built by WithCloudEventEmission.
type CEOption[T any] func(*ceEmittingTracer[T])

type eventDataTransform func(raw []byte) ([]byte, error)

// WithPayloadEncryptor seals the sensitive payload fields (input_prompt, result,
// tool_calls[].params/result, reasoning[].thinking, and per-span
// prompt_messages/completion) of every emitted event under enc before it is
// sent. Only structural fields (ids, model, agent_name, source, token counts,
// timings, exec_context, errors, prompt_hash, and the provider identifier) stay
// plaintext, so cost views keep working; the payload fields surface as opaque
// sealed envelopes to every downstream consumer — the agent-trace MCP, UI, and
// trace replayers — until a reader/break-glass Open path is deployed. Enabling
// this on a producer before that reader path ships means those consumers serve
// ciphertext, so order the rollout accordingly.
//
// Emission is fail-closed: if sealing errors (e.g. KMS is unreachable), the
// event is dropped rather than sent in the clear.
func WithPayloadEncryptor[T any](enc *payloadcrypt.Encryptor) CEOption[T] {
	return func(t *ceEmittingTracer[T]) { t.enc = enc }
}

// WithBoundedTracePayloads limits a completed-trace event to 8 MiB before
// optional payload encryption. Payload fields disabled by
// [WithPayloadsEnabled] are omitted before this limit is applied. If an enabled
// payload makes the event exceed the limit, the emitter shortens string values
// to at most 64 KiB and replaces oversized objects and arrays with empty values
// of the same JSON kind. It divides the remaining space evenly if the event is
// still too large. The trace metadata records that the payloads were truncated.
// Structural data, token counts, errors, and timings remain unchanged. If those
// fields alone exceed 8 MiB, the emitter drops the event.
//
// [WithPayloadsEnabled] independently controls the bounded request and response
// event for each model turn.
func WithBoundedTracePayloads[T any]() CEOption[T] {
	return func(t *ceEmittingTracer[T]) { t.boundTracePayloads = true }
}

// WithCloudEventEmission wraps inner so that each call to RecordTrace also
// emits the trace as a CloudEvent. The caller provides a pre-built
// cloudevents.Client (see NewBrokerClient) and a source identifier
// (e.g. the OCTO_IDENTITY of the reconciler). The CloudEvent type is
// always EventType.
//
// The event always contains structural trace fields. Raw payload fields are
// included only when the trace context enables them through WithEmitPayloads
// if set, else through WithPayloadsEnabled.
//
// Call Drain on the returned tracer (via type assertion) before process
// exit to flush in-flight events.
func WithCloudEventEmission[T any](inner Tracer[T], client cloudevents.Client, source string, opts ...CEOption[T]) Tracer[T] {
	t := &ceEmittingTracer[T]{
		inner:     inner,
		client:    client,
		source:    source,
		emissions: newEmissionCounter(),
	}
	for _, opt := range opts {
		opt(t)
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

	if err := t.setEventData(ctx, &ce, span, omitSensitiveSpanFields, nil, sealSensitiveSpanFields); err != nil {
		t.recordOutcome(ctx, SpanEventType, "encoding_failed")
		// Fail closed: on a seal error the payload would otherwise leak, so the
		// span event is dropped rather than sent in the clear.
		clog.ErrorContext(ctx, "Failed to set span CloudEvent data",
			"trace_id", span.TraceID,
			"span_id", span.SpanID,
			"error", err,
		)
		return err
	}

	t.send(ctx, ce, "Failed to deliver agent span event",
		"trace_id", span.TraceID,
		"span_id", span.SpanID,
	)
	return nil
}

// send dispatches ce on the bounded errgroup: the actual delivery happens on
// a background goroutine with retry/backoff and a timeout detached from the
// caller's cancellation, so callers return immediately under normal load and
// only block once the in-flight cap (ceMaxInflight) is reached. Every outcome
// increments the emission counter. Delivery failures are also logged with the
// identifiers from msg and logFields, but they are not returned.
func (t *ceEmittingTracer[T]) send(ctx context.Context, ce cloudevents.Event, msg string, logFields ...any) {
	t.eg.Go(func() error {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ceSendTimeout)
		defer cancel()

		rctx := cloudevents.ContextWithRetriesExponentialBackoff(sendCtx, ceRetryDelay, ceMaxRetry)
		result := t.client.Send(rctx, ce)
		outcome := "delivered"
		if cloudevents.IsUndelivered(result) {
			outcome = "undelivered"
		} else if cloudevents.IsNACK(result) {
			outcome = "nack"
		}
		t.recordOutcome(sendCtx, ce.Type(), outcome)
		if outcome != "delivered" {
			clog.ErrorContext(ctx, msg, append(logFields, "error", result)...)
		}
		return nil
	})
}

func (t *ceEmittingTracer[T]) recordOutcome(ctx context.Context, eventType, outcome string) {
	t.emissions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event_type", eventType),
		attribute.String("outcome", outcome),
	))
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

	var boundFn eventDataTransform
	if t.boundTracePayloads {
		boundFn = boundTracePayloads
	}

	if err := t.setEventData(ctx, &ce, trace, omitSensitiveTraceFields, boundFn, sealSensitiveTraceFields); err != nil {
		t.recordOutcome(ctx, EventType, "encoding_failed")
		// Drop an event that could not be prepared rather than sending data that
		// violates its payload or size policy.
		clog.ErrorContext(ctx, "Failed to set CloudEvent data",
			"trace_id", trace.ID,
			"error", err,
		)
		return
	}

	t.send(ctx, ce, "Failed to deliver agent trace event", "trace_id", trace.ID)
}

func boundTracePayloads(raw []byte) ([]byte, error) {
	if len(raw) <= maxTraceEventDataBytes {
		return raw, nil
	}

	minimum, payloadFields, err := encodeTraceWithPayloadLimit(raw, 0)
	if err != nil {
		return nil, err
	}
	if payloadFields == 0 {
		return nil, fmt.Errorf("trace is %d bytes but has no payload fields to truncate", len(raw))
	}
	if len(minimum) > maxTraceEventDataBytes {
		return nil, fmt.Errorf(
			"trace structural data is %d bytes after removing payload contents, limit is %d bytes",
			len(minimum),
			maxTraceEventDataBytes,
		)
	}
	fieldLimit := min(
		maxTracePayloadFieldBytes,
		(maxTraceEventDataBytes-len(minimum))/payloadFields,
	)
	bounded, _, err := encodeTraceWithPayloadLimit(raw, fieldLimit)
	if err != nil {
		return nil, err
	}
	if len(bounded) > maxTraceEventDataBytes {
		return nil, fmt.Errorf("bounded trace is %d bytes, limit is %d bytes", len(bounded), maxTraceEventDataBytes)
	}

	return bounded, nil
}

func encodeTraceWithPayloadLimit(raw []byte, fieldLimit int) ([]byte, int, error) {
	payloadFields := 0
	limitField := func(obj map[string]json.RawMessage, key string) error {
		value, ok := obj[key]
		if !ok {
			return nil
		}
		obj[key] = limitJSONValue(value, fieldLimit)
		payloadFields++
		return nil
	}

	nested := fieldTransforms{
		jsonValue:   limitField,
		stringValue: limitField,
	}
	transforms := fieldTransforms{
		jsonValue:   nested.jsonValue,
		stringValue: nested.stringValue,
		reasoning:   reasoningFieldTransform(nested),
	}

	bounded, err := transformSensitiveTraceFields(raw, "bounded", transforms)
	if err != nil {
		return nil, 0, err
	}

	var event map[string]json.RawMessage
	if err := json.Unmarshal(bounded, &event); err != nil {
		return nil, 0, fmt.Errorf("unmarshal bounded trace: %w", err)
	}

	var metadata map[string]json.RawMessage
	if metadataJSON, ok := event["metadata"]; ok {
		if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
			return nil, 0, fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		metadata = map[string]json.RawMessage{}
	}
	metadata[payloadTruncatedMetadataField] = json.RawMessage("true")
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal metadata: %w", err)
	}
	event["metadata"] = encodedMetadata

	bounded, err = json.Marshal(event)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal bounded trace: %w", err)
	}

	return bounded, payloadFields, nil
}

func limitJSONValue(value json.RawMessage, maxBytes int) json.RawMessage {
	if len(value) <= maxBytes {
		return value
	}

	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return value
	}

	if trimmed[0] == '{' {
		return json.RawMessage(`{}`)
	}
	if trimmed[0] == '[' {
		return json.RawMessage(`[]`)
	}
	if trimmed[0] == '-' || trimmed[0] >= '0' && trimmed[0] <= '9' {
		return json.RawMessage(`0`)
	}
	if trimmed[0] != '"' {
		return value
	}

	if maxBytes < 2 {
		return json.RawMessage(`""`)
	}

	var stringValue string
	if err := json.Unmarshal(trimmed, &stringValue); err != nil {
		return value
	}

	low, high := 0, min(len(stringValue), maxBytes-2)
	best := json.RawMessage(`""`)
	for low <= high {
		end := low + (high-low)/2
		candidate, err := json.Marshal(strings.ToValidUTF8(stringValue[:end], ""))
		if err != nil || len(candidate) > maxBytes {
			high = end - 1
			continue
		}
		best = candidate
		low = end + 1
	}

	return best
}

// setEventData serializes obj, removes sensitive fields unless payloads are
// enabled, applies boundFn when one is configured, and then seals enabled
// payloads when an encryptor is present. Any error prevents event emission.
//
// Sealing runs on a context detached from the caller's, the same treatment send
// gives delivery. It is a bounded amount of work — one KMS wrap per event, the
// rest local AES-GCM — and the caller is very often a reconcile whose context
// has just been canceled (an instance drain, a dispatcher teardown, a deadline),
// which is exactly when the record of what the agent did is most worth keeping.
// Inheriting that cancellation meant the KMS Encrypt was rejected before it left
// the process (codes.Canceled, which the KMS client deliberately does not retry)
// and every trace on a canceled run was dropped fail-closed. The timeout bounds
// how long an unwinding caller can be held here. The omit path needs none of
// this: it makes no KMS call.
func (t *ceEmittingTracer[T]) setEventData(
	ctx context.Context,
	ce *cloudevents.Event,
	obj any,
	omitFn func(raw []byte) ([]byte, error),
	boundFn eventDataTransform,
	sealFn func(ctx context.Context, enc *payloadcrypt.Encryptor, raw []byte) ([]byte, error),
) error {
	payloadsEnabled := emitPayloadsEnabledFrom(ctx)
	raw, err := json.Marshal(obj)
	if err != nil {
		if !payloadsEnabled {
			return errTraceMarshal
		}

		return err
	}
	if !payloadsEnabled {
		raw, err = omitFn(raw)
		if err != nil {
			return err
		}
	}
	if boundFn != nil {
		raw, err = boundFn(raw)
		if err != nil {
			return err
		}
	}
	if payloadsEnabled && t.enc != nil {
		sealCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ceSealTimeout)
		defer cancel()

		raw, err = sealFn(sealCtx, t.enc, raw)
		if err != nil {
			return err
		}
	}
	return ce.SetData(cloudevents.ApplicationJSON, raw)
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
