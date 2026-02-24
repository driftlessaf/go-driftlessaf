/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package graphqlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v75/github"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/shurcooL/githubv4"
)

// newTestClient creates a GraphQLClient backed by a test HTTP server.
// The handler controls what the "GitHub GraphQL API" returns.
func newTestClient(t *testing.T, handler http.Handler) *GraphQLClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	gh := github.NewClient(nil).WithAuthToken("fake")
	return newGraphQLClient(gh, srv.URL)
}

// counterValue returns the current value of a counter with the given label set,
// or -1 if no matching series exists.
func counterValue(cv *prometheus.CounterVec, labels prometheus.Labels) float64 {
	c, err := cv.GetMetricWith(labels)
	if err != nil {
		return -1
	}
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		return -1
	}
	return m.GetCounter().GetValue()
}

func TestQuery_RecordsHTTPStatusCode(t *testing.T) {
	// Reset metrics so parallel tests don't interfere.
	mGraphQLOperations.Reset()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))

	var q struct{}
	err := client.Query(context.Background(), "TestOp", &q, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := counterValue(mGraphQLOperations, prometheus.Labels{
		"operation":      "TestOp",
		"operation_type": "query",
		"status":         "success",
		"response_code":  "200",
	})
	if got != 1 {
		t.Errorf("expected counter=1 for {operation=TestOp, operation_type=query, status=success, response_code=200}, got %v", got)
	}
}

func TestQuery_RecordsHTTPStatusCodeOnError(t *testing.T) {
	mGraphQLOperations.Reset()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))

	var q struct{}
	err := client.Query(context.Background(), "RateLimitedOp", &q, nil)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}

	got := counterValue(mGraphQLOperations, prometheus.Labels{
		"operation":      "RateLimitedOp",
		"operation_type": "query",
		"status":         "error",
		"response_code":  "403",
	})
	if got != 1 {
		t.Errorf("expected counter=1 for {operation=RateLimitedOp, operation_type=query, status=error, response_code=403}, got %v", got)
	}
}

func TestMutate_RecordsHTTPStatusCode(t *testing.T) {
	mGraphQLOperations.Reset()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))

	var m struct {
		AddStar struct {
			Starrable struct {
				ID githubv4.ID
			}
		} `graphql:"addStar(input: $input)"`
	}
	input := githubv4.AddStarInput{StarrableID: githubv4.ID("test")}
	err := client.Mutate(context.Background(), "MutateOp", &m, input, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := counterValue(mGraphQLOperations, prometheus.Labels{
		"operation":      "MutateOp",
		"operation_type": "mutation",
		"status":         "success",
		"response_code":  "200",
	})
	if got != 1 {
		t.Errorf("expected counter=1 for {operation=MutateOp, operation_type=mutation, status=success, response_code=200}, got %v", got)
	}
}

// Each request through the transport must write its status code to its
// own context-provided pointer, not to shared state. This is tested
// sequentially — if the transport used a single shared field, request B
// would overwrite request A's value before A reads it. The context-based
// design makes each request's storage independent.
func TestStatusCapturingTransport_IsolatesPerRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-ID") == "A" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	t.Cleanup(srv.Close)

	transport := &statusCapturingTransport{base: http.DefaultTransport}

	var codeA, codeB int

	ctxA := context.WithValue(context.Background(), statusCodeKey{}, &codeA)
	ctxB := context.WithValue(context.Background(), statusCodeKey{}, &codeB)

	// Request A completes.
	reqA, _ := http.NewRequestWithContext(ctxA, "GET", srv.URL, nil)
	reqA.Header.Set("X-Test-ID", "A")
	respA, err := transport.RoundTrip(reqA)
	if err != nil {
		t.Fatalf("request A: %v", err)
	}
	respA.Body.Close()

	// Request B completes — with shared state, this would clobber A's code.
	reqB, _ := http.NewRequestWithContext(ctxB, "GET", srv.URL, nil)
	reqB.Header.Set("X-Test-ID", "B")
	respB, err := transport.RoundTrip(reqB)
	if err != nil {
		t.Fatalf("request B: %v", err)
	}
	respB.Body.Close()

	// A's code must still be 200 even though B's request happened after.
	if codeA != http.StatusOK {
		t.Errorf("request A: got status %d, want %d (clobbered by subsequent request)", codeA, http.StatusOK)
	}
	if codeB != http.StatusTooManyRequests {
		t.Errorf("request B: got status %d, want %d", codeB, http.StatusTooManyRequests)
	}
}

func TestQuery_RecordsHTTPStatusCodeOnConnectionError(t *testing.T) {
	mGraphQLOperations.Reset()
	mGraphQLDuration.Reset()

	// Create a client pointing at a server that's already closed,
	// so the HTTP request itself fails (no response code at all).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	gh := github.NewClient(nil).WithAuthToken("fake")
	client := newGraphQLClient(gh, url)

	var q struct{}
	err := client.Query(context.Background(), "ConnFailOp", &q, nil)
	if err == nil {
		t.Fatal("expected error for connection failure")
	}

	// With no HTTP response, response_code should be "0"
	got := counterValue(mGraphQLOperations, prometheus.Labels{
		"operation":      "ConnFailOp",
		"operation_type": "query",
		"status":         "error",
		"response_code":  "0",
	})
	if got != 1 {
		t.Errorf("expected counter=1 for {operation=ConnFailOp, operation_type=query, status=error, response_code=0}, got %v", got)
	}
}
