/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/genai"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsRetryable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"genai 429", genai.APIError{Code: http.StatusTooManyRequests}, true},
		{"genai 503 pointer", &genai.APIError{Code: http.StatusServiceUnavailable}, true},
		{"genai 400", genai.APIError{Code: http.StatusBadRequest}, false},
		{"genai 404", fmt.Errorf("wrapped: %w", genai.APIError{Code: http.StatusNotFound}), false},
		{"storage 503", &googleapi.Error{Code: http.StatusServiceUnavailable}, true},
		{"storage 403", &googleapi.Error{Code: http.StatusForbidden}, false},
		{"overloaded line", &lineError{code: statusOverloaded, message: "overloaded_error: Overloaded"}, true},
		{"bad line", &lineError{code: -1, message: "invalid_request_error"}, false},
		{"line that never ran", &lineError{code: -1, message: "request expired", transient: true}, true},
		{"truncated read", io.ErrUnexpectedEOF, true},
		{"url timeout", &url.Error{Op: "Get", URL: "https://x", Err: timeoutErr{}}, true},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"sentinel", ErrMissing, false},
		{"invalid request quoting a status keyword", fmt.Errorf("%w: writes to %q", ErrInvalidRequest, "gs://x/UNAVAILABLE/"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRetryable(tt.err); got != tt.want {
				t.Errorf("IsRetryable(%v): got = %v, want = %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNewClientRejects(t *testing.T) {
	t.Parallel()
	vertex, err := genai.NewClient(t.Context(), &genai.ClientConfig{Backend: genai.BackendVertexAI, Project: "p", Location: "us", HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	gemini, err := genai.NewClient(t.Context(), &genai.ClientConfig{Backend: genai.BackendGeminiAPI, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := storage.NewClient(t.Context(), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	bucket := sc.Bucket(testBucket)
	tests := []struct {
		name   string
		gc     *genai.Client
		bucket *storage.BucketHandle
		opts   []Option
	}{
		{name: "nil genai", bucket: bucket},
		{name: "gemini api backend", gc: gemini, bucket: bucket},
		{name: "nil bucket", gc: vertex},
		{name: "nil metrics", gc: vertex, bucket: bucket, opts: []Option{WithMetrics(nil)}},
		{name: "negative retries", gc: vertex, bucket: bucket, opts: []Option{WithRetryConfig(retry.RetryConfig{MaxRetries: -1})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewClient(tt.gc, tt.bucket, "p", tt.opts...); err == nil {
				t.Error("NewClient: got nil error")
			}
		})
	}
}

// TestResultsRecordsMetrics swaps the global meter provider, so it must not
// run in parallel with other tests.
func TestResultsRecordsMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	env := newTestEnv(t, WithMetrics(metrics.NewGenAI(t.Name())), WithResourceLabels(map[string]string{"service_name": "batch-test"}))
	keys := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	job, err := env.client.SubmitClaude(t.Context(), uuid.NewString(), "claude-opus-5-5@default",
		[]ClaudeRequest{claudeReq(keys[0], "a"), claudeReq(keys[1], "b"), claudeReq(keys[2], "c")})
	if err != nil {
		t.Fatalf("SubmitClaude: %v", err)
	}
	env.vertex.finish(t, env.gcs, job.Name, genai.JobStatePartiallySucceeded,
		fmt.Sprintf(`{"custom_id":%q,"response":%s}`, ref(keys[0]), claudeMessage("a", "end_turn", "")),
		fmt.Sprintf(`{"custom_id":%q,"result":{"type":"errored","error":{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}}}`, ref(keys[1])))
	for _, err := range env.client.Results(t.Context(), job) {
		if err != nil {
			t.Fatalf("Results: %v", err)
		}
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	sums := map[string]int64{}
	operations := map[string]struct{}{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					checkAttr(t, m.Name, p.Attributes, "gen_ai.provider.name", "gcp.vertex_ai")
					checkAttr(t, m.Name, p.Attributes, "model", "claude-opus-5-5@default")
					checkAttr(t, m.Name, p.Attributes, "service_name", "batch-test")
					name := m.Name
					if v, ok := p.Attributes.Value("response_code"); ok {
						name += "/" + v.AsString()
					}
					if v, ok := p.Attributes.Value("gen_ai.token.type"); ok {
						name += "/" + v.AsString()
					}
					sums[name] += p.Value
				}
			case metricdata.Histogram[float64]:
				if m.Name != "gen_ai.client.token.usage" {
					continue
				}
				for _, p := range d.DataPoints {
					v, _ := p.Attributes.Value("gen_ai.operation.name")
					operations[v.AsString()] = struct{}{}
				}
			}
		}
	}
	want := map[string]int64{
		// One success and one overloaded line; the missing key made no request.
		"genai.api.requests/200":            1,
		"genai.api.requests/529":            1,
		"genai.token.prompt":                100,
		"genai.token.completion":            20,
		"genai.token.prompt/cache_read":     7,
		"genai.token.prompt/cache_creation": 3,
	}
	for k, v := range want {
		if sums[k] != v {
			t.Errorf("%s: got = %d, want = %d", k, sums[k], v)
		}
	}
	if _, ok := operations["batch_predict"]; !ok || len(operations) != 1 {
		t.Errorf("gen_ai.operation.name: got = %v, want only batch_predict", operations)
	}
}

func checkAttr(t *testing.T, metric string, set attribute.Set, key, want string) {
	t.Helper()
	if got, _ := set.Value(attribute.Key(key)); got.AsString() != want {
		t.Errorf("%s %s: got = %q, want = %q", metric, key, got.AsString(), want)
	}
}
