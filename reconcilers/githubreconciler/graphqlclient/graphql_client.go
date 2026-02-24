/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package graphqlclient

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v75/github"
	"github.com/shurcooL/githubv4"
)

type statusCodeKey struct{}

// statusCapturingTransport wraps an http.RoundTripper and writes the HTTP
// status code into the request's context. Each request carries its own
// *int via the statusCodeKey context value, so concurrent requests on the
// same transport never interfere with each other.
//
// This workaround exists because the shurcooL/graphql client discards HTTP
// status codes, embedding them only as unstructured text in fmt.Errorf
// strings. If https://github.com/shurcooL/graphql/pull/126 is merged,
// this transport wrapper can be replaced by a type assertion on the error.
type statusCapturingTransport struct {
	base http.RoundTripper
}

func (t *statusCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil {
		if p, ok := req.Context().Value(statusCodeKey{}).(*int); ok {
			*p = resp.StatusCode
		}
	}
	return resp, err
}

// GraphQLClient is a thin wrapper around the shurcooL githubv4 client that
// records metrics for each operation. Callers define their own query/mutation
// structs and provide an operation name for metrics tracking.
//
// GraphQLClient is safe for concurrent use.
type GraphQLClient struct {
	client *githubv4.Client
}

// NewGraphQLClient creates a new GraphQL client from a github.Client.
// It reuses the underlying HTTP client for authentication.
func NewGraphQLClient(gh *github.Client) *GraphQLClient {
	return newGraphQLClient(gh, "")
}

// newGraphQLClient creates a GraphQL client, optionally targeting a custom
// endpoint (for testing). If url is empty, the default GitHub API is used.
func newGraphQLClient(gh *github.Client, url string) *GraphQLClient {
	origClient := gh.Client()

	base := origClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}

	// Wrap in a new http.Client to avoid mutating the caller's.
	httpClient := &http.Client{
		Transport: &statusCapturingTransport{base: base},
		Timeout:   origClient.Timeout,
	}

	var client *githubv4.Client
	if url != "" {
		client = githubv4.NewEnterpriseClient(url, httpClient)
	} else {
		client = githubv4.NewClient(httpClient)
	}

	return &GraphQLClient{client: client}
}

// query calls the underlying client with a context that carries a status
// code pointer, then returns the captured code alongside the error.
func (c *GraphQLClient) query(ctx context.Context, q any, variables map[string]any) (int, error) {
	var code int
	ctx = context.WithValue(ctx, statusCodeKey{}, &code)
	err := c.client.Query(ctx, q, variables)
	return code, err
}

// mutate calls the underlying client with a context that carries a status
// code pointer, then returns the captured code alongside the error.
func (c *GraphQLClient) mutate(ctx context.Context, m any, input githubv4.Input, variables map[string]any) (int, error) {
	var code int
	ctx = context.WithValue(ctx, statusCodeKey{}, &code)
	err := c.client.Mutate(ctx, m, input, variables)
	return code, err
}

// Query executes a GraphQL query and records metrics.
// `operationName` is used to label metrics for observability.
// It must be a static string representing the actual name of the GraphQL operation.
// Do not include dynamic content, as this will cause a cardinality explosion that will break our metrics.
func (c *GraphQLClient) Query(ctx context.Context, operationName string, q any, variables map[string]any) error {
	start := time.Now()

	code, err := c.query(ctx, q, variables)

	status := "success"
	if err != nil {
		status = "error"
	}
	mGraphQLOperations.WithLabelValues(operationName, status, strconv.Itoa(code)).Inc()
	mGraphQLDuration.WithLabelValues(operationName).Observe(time.Since(start).Seconds())

	return err
}

// Mutate executes a GraphQL mutation and records metrics.
// The operationName is used to label metrics for observability.
// It must be a static string representing the actual name of the GraphQL operation.
// Do not include dynamic content, as this will cause a cardinality explosion that will break our metrics.
func (c *GraphQLClient) Mutate(ctx context.Context, operationName string, m any, input githubv4.Input, variables map[string]any) error {
	start := time.Now()

	code, err := c.mutate(ctx, m, input, variables)

	status := "success"
	if err != nil {
		status = "error"
	}
	mGraphQLOperations.WithLabelValues(operationName, status, strconv.Itoa(code)).Inc()
	mGraphQLDuration.WithLabelValues(operationName).Observe(time.Since(start).Seconds())

	return err
}
