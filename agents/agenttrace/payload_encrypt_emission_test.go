/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace/payloadcrypt"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
)

// captureServer records every CloudEvent body the tracer sends. Returns the
// server, a CE client targeting it, and an accessor for the received bodies.
func captureServer(t *testing.T) (*httptest.Server, cloudevents.Client, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(srv.URL),
		cehttp.WithClient(*srv.Client()),
	)
	if err != nil {
		srv.Close()
		t.Fatalf("creating test CE client: %v", err)
	}
	return srv, client, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]byte, len(bodies))
		copy(out, bodies)
		return out
	}
}

func xorEncryptor(t *testing.T) *payloadcrypt.Encryptor {
	t.Helper()
	enc, err := payloadcrypt.New("projects/p/locations/l/keyRings/r/cryptoKeys/k", func(_ context.Context, dek []byte) ([]byte, error) {
		out := make([]byte, len(dek))
		for i, b := range dek {
			out[i] = b ^ 0x5a
		}
		return out, nil
	})
	if err != nil {
		t.Fatalf("payloadcrypt.New: %v", err)
	}
	return enc
}

// runTraceWithTurn drives one trace with a payload-recording turn against the
// given tracer, then drains in-flight sends.
func runTraceWithTurn(t *testing.T, wrapped Tracer[string]) *Trace[string] {
	t.Helper()
	ctx := WithPayloadsEnabled(t.Context(), true)
	ctx = WithExecutionContext(ctx, ExecutionContext{ReconcilerKey: "pr:owner/repo/42"})
	ctx = WithTracer[string](ctx, wrapped)

	trace, done := StartTrace[string](ctx, "secret prompt body")
	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "secret prompt body"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := turn.RecordResponse(map[string]string{"content": "secret completion body"}); err != nil {
		t.Fatalf("RecordResponse: %v", err)
	}
	turn.End()

	// Record a tool call and a reasoning block so the emission tests exercise the
	// tool_calls[].params/result and reasoning[].thinking sealing paths against
	// real trace marshalling — not just the hand-written fixture in
	// payload_encrypt_test.go. A JSON-tag rename that silently un-sealed these
	// fields would leak plaintext here and fail TestWithPayloadEncryptor_SealsEmittedBodies.
	tc := trace.StartToolCall("call-1", "edit_file", map[string]any{"path": "secret param body"})
	tc.Complete(map[string]any{"stdout": "secret toolresult body"}, nil)
	trace.Reasoning = []ReasoningContent{{Thinking: "secret reasoning body"}}

	done("secret result body", nil)
	drainCE[string](wrapped)
	return trace
}

// With an encryptor configured, both the per-span and per-trace CloudEvents must
// carry sealed ciphertext — no plaintext payload appears on the wire.
func TestWithPayloadEncryptor_SealsEmittedBodies(t *testing.T) {
	srv, client, bodiesFn := captureServer(t)
	defer srv.Close()

	inner := ByCode[string](func(_ *Trace[string]) {})
	wrapped := WithCloudEventEmission[string](inner, client, "test-source",
		WithPayloadEncryptor[string](xorEncryptor(t)))

	runTraceWithTurn(t, wrapped)

	bodies := bodiesFn()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 CloudEvents (1 span + 1 trace), got %d", len(bodies))
	}
	// The sealing marker must be present, and no plaintext payload may leak.
	plaintexts := [][]byte{
		[]byte("secret prompt body"),
		[]byte("secret completion body"),
		[]byte("secret result body"),
		[]byte("secret param body"),
		[]byte("secret toolresult body"),
		[]byte("secret reasoning body"),
	}
	for i, body := range bodies {
		if !bytes.Contains(body, []byte("driftlessaf_enc")) {
			t.Errorf("event %d not sealed (no envelope marker): %s", i, body)
		}
		for _, pt := range plaintexts {
			if bytes.Contains(body, pt) {
				t.Errorf("event %d leaked plaintext %q: %s", i, pt, body)
			}
		}
	}
}

// The security promise: when sealing fails (e.g. KMS unreachable), emission must
// be fail-closed — the event is dropped, never sent in the clear. A regression
// that swallowed the seal error and still called ce.SetData would send plaintext
// and this test would catch it.
func TestWithPayloadEncryptor_FailClosedDropsEvents(t *testing.T) {
	srv, client, bodiesFn := captureServer(t)
	defer srv.Close()

	failing, err := payloadcrypt.New("projects/p/locations/l/keyRings/r/cryptoKeys/k",
		func(context.Context, []byte) ([]byte, error) {
			return nil, io.ErrUnexpectedEOF // stand-in for a KMS wrap failure
		})
	if err != nil {
		t.Fatalf("payloadcrypt.New: %v", err)
	}

	inner := ByCode[string](func(_ *Trace[string]) {})
	wrapped := WithCloudEventEmission[string](inner, client, "test-source",
		WithPayloadEncryptor[string](failing))

	runTraceWithTurn(t, wrapped)

	if got := bodiesFn(); len(got) != 0 {
		t.Fatalf("fail-closed violated: expected 0 CloudEvents when sealing errors, got %d: %s", len(got), bytes.Join(got, []byte("\n---\n")))
	}
}
