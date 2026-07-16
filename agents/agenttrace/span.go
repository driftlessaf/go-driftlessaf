/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SpanEventType is the CloudEvent type for the per-span trace payload. One
// event is emitted per LLM turn when payloads are enabled, alongside the
// per-trace event emitted at trace completion.
//
// Per-span emission is gated on WithPayloadsEnabled(ctx, true). The gate
// is an explicit caller opt-in because the per-span payload carries the
// full prompt and completion bytes alongside the per-turn metadata that
// the per-trace event already records. When the gate is off (the
// default), RecordRequest and RecordResponse are no-ops, no per-span
// CloudEvent is emitted, and only the per-trace event flows. When the
// gate is on, the caller takes responsibility for what ends up in the
// receiving BigQuery table.
//
// # Retention
//
// Persisted payloads are stored verbatim. Retention is configured by the
// receiving cloudevent-recorder module, which sets BigQuery partition
// expiration on the table this event lands in. Two knobs:
//
//   - Dataset default: set retention-period (in days) on the
//     cloudevent-recorder module call. Applies to every CloudEvent type
//     the module records.
//   - Per-event override: set retention_period_days on the
//     "dev.chainguard.driftlessaf.agent.span.v1" entry in the module's
//     types map. Overrides the dataset default for this event type only.
//
// The partition field is recorded_at (see
// iac/schemas/agent_trace_span.schema.json).
const SpanEventType = "dev.chainguard.driftlessaf.agent.span.v1"

// RecordedSpan is one LLM turn's payload, shaped to the agent_trace_span
// BigQuery schema (see iac/schemas/agent_trace_span.schema.json). Field names
// mirror the BQ column names exactly via json tags. JSON columns use
// json.RawMessage so the marshal is pass-through and the BQ ingester treats
// them as native JSON instead of nested STRING.
//
// SpanID is deterministic (trace_id + turn_index) so a CloudEvent retry that
// re-delivers the same event produces a row with the same span_id. BigQuery
// does not enforce uniqueness on this field; downstream consumers that need
// at-most-once semantics must dedup by span_id themselves.
type RecordedSpan struct {
	TraceID        string          `json:"trace_id"`
	SpanID         string          `json:"span_id"`
	AgentName      string          `json:"agent_name,omitempty"`
	ModelID        string          `json:"model_id,omitempty"`
	RecordedAt     time.Time       `json:"recorded_at"`
	PromptMessages json.RawMessage `json:"prompt_messages,omitempty"`
	Completion     json.RawMessage `json:"completion,omitempty"`
	PromptHash     string          `json:"prompt_hash,omitempty"`
	TokenCounts    json.RawMessage `json:"token_counts,omitempty"`
	JudgeScore     *float64        `json:"judge_score,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

// SpanEmitter is the callback used by LLMTurn.End to ship a per-turn payload
// out as a CloudEvent. Implementations should be non-blocking — the existing
// per-trace emitter pattern uses a bounded errgroup, and span emission must
// not stall the reconciler.
type SpanEmitter func(ctx context.Context, span RecordedSpan) error

// RecordRequest captures the per-turn prompt messages. When
// WithPayloadsEnabled is false on the trace context this is a no-op returning
// nil. The payload is truncated to maxPayloadBytes before being stored on
// the turn for later emission in End.
//
// Payloads are persisted verbatim — see the "Security posture" block on
// [SpanEventType] for the implications and the canonical handling of a
// credential-leak event.
func (lt *LLMTurn[T]) RecordRequest(messages any) error {
	if !payloadsEnabledFrom(lt.trace.ctx) {
		return nil
	}
	payload, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	lt.requestPayload, lt.requestTruncated = preparePayload(payload)
	return nil
}

// RecordResponse captures the per-turn completion content. Same gating and
// truncation as RecordRequest; same verbatim-persistence posture — see the
// "Security posture" block on [SpanEventType].
func (lt *LLMTurn[T]) RecordResponse(content any) error {
	if !payloadsEnabledFrom(lt.trace.ctx) {
		return nil
	}
	payload, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	lt.responsePayload, lt.responseTruncated = preparePayload(payload)
	return nil
}

// preparePayload caps the JSON payload at maxPayloadBytes while preserving
// validity. When the input fits, the bytes flow straight into the
// `prompt_messages` / `completion` columns as native JSON. When it exceeds
// the cap, the original bytes are cut mid-structure (producing invalid JSON)
// and re-encoded as a JSON string literal — that guarantees the value passed
// to json.RawMessage is always valid JSON so the CloudEvent emit downstream
// cannot fail. didTruncate is returned so the caller can mark the span's
// metadata with driftlessaf.payload.truncated = true, matching the OTel
// per-trace attribute set in NewTrace.
//
// JSON-encoding a string adds two delimiter bytes (the surrounding quotes)
// plus one escape byte for every backslash, quote, control char, etc. in
// the content. To keep the final encoded payload at or below maxPayloadBytes
// the truncator iteratively shrinks the source until the encoded form fits.
// In the common case (no special characters) one pass is enough.
//
// Not a duplicate of truncatePayload (trace.go): that is the raw byte cut
// used for OTel span attributes, which this wraps with the JSON-validity
// guarantee described above.
func preparePayload(payload []byte) (bytes []byte, didTruncate bool) {
	if len(payload) <= maxPayloadBytes {
		return payload, false
	}
	// Cut the raw bytes — invalid JSON, but a starting point for the
	// re-encoding loop below.
	truncated, _ := truncatePayload(string(payload))
	for {
		encoded, err := json.Marshal(truncated)
		if err != nil {
			// Defensive: json.Marshal of a string cannot fail. Fall back to
			// the raw truncated bytes; the metadata flag still signals the
			// drop to downstream consumers.
			return []byte(truncated), true
		}
		if len(encoded) <= maxPayloadBytes {
			return encoded, true
		}
		// Encoded form overshot (escapes inflated the length). Shrink the
		// source by the overshoot and retry. Guard against an empty source
		// — if escapes alone would exceed the cap the loop would spin.
		overshoot := len(encoded) - maxPayloadBytes
		if overshoot >= len(truncated) {
			return []byte(`""`), true
		}
		truncated = truncated[:len(truncated)-overshoot]
	}
}

// hashPrompt returns the hex SHA-256 of payload. Used as prompt_hash for
// grouping equivalent prompt versions across runs.
func hashPrompt(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// buildRecordedSpan assembles the per-turn payload from the LLMTurn state.
// Returns ok=false when there is nothing worth emitting (no request payload
// was recorded — without the prompt, the row carries no analytical value).
func (lt *LLMTurn[T]) buildRecordedSpan() (RecordedSpan, bool) {
	if len(lt.requestPayload) == 0 {
		return RecordedSpan{}, false
	}

	tokenCounts, _ := json.Marshal(map[string]int64{
		"input":       lt.record.InputTokens,
		"output":      lt.record.OutputTokens,
		"cache_read":  lt.record.CacheReadTokens,
		"cache_write": lt.record.CacheCreationTokens,
	})

	execCtx := lt.trace.ExecContext
	metaMap := map[string]any{
		"trace_id":        lt.trace.ID,
		"otel_trace_id":   lt.trace.OTelTraceID,
		"reconciler_key":  execCtx.ReconcilerKey,
		"reconciler_type": execCtx.ReconcilerType,
		"commit_sha":      execCtx.CommitSHA,
		"turn_index":      lt.record.Index,
		"system":          lt.record.System,
	}
	// Surface payload truncation alongside the OTel per-trace attribute set
	// at trace.go (driftlessaf.payload.truncated = true) so BigQuery rows
	// where the prompt or completion was truncated are identifiable without
	// joining against the OTel span data.
	if lt.requestTruncated || lt.responseTruncated {
		metaMap["driftlessaf.payload.truncated"] = true
	}
	metadata, _ := json.Marshal(metaMap)

	return RecordedSpan{
		TraceID:        lt.trace.ID,
		SpanID:         fmt.Sprintf("%s-t%d", lt.trace.ID, lt.record.Index),
		AgentName:      lt.trace.AgentName,
		ModelID:        lt.record.Model,
		RecordedAt:     lt.record.EndTime,
		PromptMessages: lt.requestPayload,
		Completion:     lt.responsePayload,
		PromptHash:     hashPrompt(lt.requestPayload),
		TokenCounts:    tokenCounts,
		Metadata:       metadata,
	}, true
}
