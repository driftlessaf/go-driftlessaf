/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package bedrockruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/agents/awsauth"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const maxRequestBytes = 25_000_000

var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// Client exposes credential-free operations for one Bedrock Runtime endpoint.
// Clients returned by New are safe for concurrent use.
type Client interface {
	// Endpoint returns the regional HTTPS origin for adapter API paths.
	Endpoint() string
	// Do signs and sends a request without redirects. It consumes and closes
	// the request body without modifying the caller's URL or headers. The
	// returned response has sanitized request metadata and a streaming body
	// that the caller must close. HTTP error responses remain available for
	// protocol handling.
	Do(*http.Request) (*http.Response, error)
	// CloseIdleConnections releases pooled connections without interrupting requests.
	CloseIdleConnections()
}

type client struct {
	region      string
	host        string
	credentials aws.CredentialsProvider
	transport   http.RoundTripper
	now         func() time.Time
}

var _ Client = (*client)(nil)

// New validates cfg and binds its refreshable credential provider to a client.
// It does not invoke a model. Credential discovery may contact AWS SSO or STS.
func New(ctx context.Context, cfg awsauth.Config) (Client, error) {
	if !ValidRegion(cfg.Region) {
		return nil, errors.New("bedrock runtime: invalid or unsupported region")
	}
	awsConfig, err := cfg.LoadAWSConfig(privateTraceContext{ctx})
	if err != nil {
		return nil, safeError("credential validation", err)
	}
	transport, err := newClient(awsConfig, &http.Transport{
		// Deliberately do not inherit ProxyFromEnvironment or DefaultTransport.
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}, time.Now)
	if err != nil {
		// Do not return a typed nil through the Client interface.
		return nil, err
	}
	return transport, nil
}

func newClient(cfg aws.Config, transport http.RoundTripper, now func() time.Time) (*client, error) {
	if !ValidRegion(cfg.Region) || cfg.Credentials == nil || transport == nil || now == nil {
		return nil, errors.New("bedrock runtime: invalid client configuration")
	}
	return &client{
		region: cfg.Region, host: "bedrock-runtime." + cfg.Region + ".amazonaws.com",
		credentials: cfg.Credentials, transport: transport, now: now,
	}, nil
}

// ValidRegion reports whether region has the syntax and partition supported by
// this transport. It does not verify regional model availability or account access.
// Adapters can use it before credential discovery.
func ValidRegion(region string) bool {
	return len(region) <= 63 && regionPattern.MatchString(region) &&
		!strings.HasPrefix(region, "cn-") && !strings.HasPrefix(region, "us-gov-") &&
		!strings.HasPrefix(region, "us-iso") && !strings.HasPrefix(region, "eu-isoe-")
}

// Endpoint returns the regional HTTPS origin. The adapter appends its API path.
func (c *client) Endpoint() string { return "https://" + c.host }

// Do signs and sends a request without following redirects. As with http.Client,
// Do consumes and closes the request body. It leaves the request's URL and
// headers unchanged. The returned Response.Request contains no signed headers
// or replayable body. HTTP error responses are returned for protocol handling.
func (c *client) Do(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("bedrock runtime: request is nil")
	}
	ctx := privateTraceContext{request.Context()}
	if request.Body != nil {
		// Request.Body must allow Close to unblock a concurrent Read, as required
		// by net/http. Honor cancellation while buffering, not only on the wire.
		closeBody := sync.OnceFunc(func() { request.Body.Close() })
		stop := context.AfterFunc(ctx, closeBody)
		defer func() {
			stop()
			closeBody()
		}()
	}
	if request.URL == nil || request.URL.Scheme != "https" || request.URL.Host != c.host ||
		request.URL.User != nil || request.URL.Opaque != "" || request.URL.RawQuery != "" ||
		request.URL.ForceQuery || request.URL.Fragment != "" || request.RequestURI != "" ||
		(request.Host != "" && request.Host != c.host) {
		return nil, errors.New("bedrock runtime: request must target the configured HTTPS origin without credentials or query parameters")
	}
	if err := ctx.Err(); err != nil {
		return nil, safeError("request", err)
	}
	if request.ContentLength > maxRequestBytes {
		return nil, errors.New("bedrock runtime: request exceeds the 25 MB limit")
	}
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
		if err != nil {
			if ctx.Err() != nil {
				return nil, safeError("request body read", ctx.Err())
			}
			return nil, safeError("request body read", err)
		}
	}
	if len(body) > maxRequestBytes {
		return nil, errors.New("bedrock runtime: request exceeds the 25 MB limit")
	}
	credentials, err := c.credentials.Retrieve(ctx)
	if err != nil {
		return nil, safeError("credential refresh", err)
	}
	if credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" || credentials.SessionToken == "" {
		return nil, errors.New("bedrock runtime: temporary AWS credentials are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, safeError("request", err)
	}
	signed := request.Clone(ctx)
	signed.Header = cleanHeaders(request.Header)
	for _, key := range []string{"Host", "Content-Length", "Transfer-Encoding", "Trailer"} {
		signed.Header.Del(key)
	}
	signed.Host = c.host
	signed.Body = io.NopCloser(bytes.NewReader(body))
	signed.GetBody = nil
	signed.ContentLength = int64(len(body))
	signed.TransferEncoding = nil
	signed.Trailer = nil
	hash := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(ctx, credentials, signed, hex.EncodeToString(hash[:]), "bedrock", c.region, c.now()); err != nil {
		return nil, safeError("request signing", err)
	}
	response, err := c.transport.RoundTrip(signed)
	if err != nil {
		return nil, safeError("HTTP request", err)
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		response.Body.Close()
		return nil, errors.New("bedrock runtime: redirects are not permitted")
	}
	response.Request = request.Clone(ctx)
	response.Request.Header = cleanHeaders(request.Header)
	response.Request.Body = nil
	response.Request.GetBody = nil
	response.Header = cleanHeaders(response.Header)
	return response, nil
}

// CloseIdleConnections releases pooled connections without interrupting requests.
func (c *client) CloseIdleConnections() {
	if transport, ok := c.transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

func cleanHeaders(headers http.Header) http.Header {
	clean := make(http.Header, len(headers))
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "authorization", "proxy-authorization", "x-api-key", "api-key", "cookie", "cookie2", "connection":
			continue
		}
		if strings.HasPrefix(strings.ToLower(key), "x-amz-") {
			continue
		}
		for _, value := range values {
			clean.Add(key, value)
		}
	}
	return clean
}

// Suppress header-bearing httptrace callbacks without losing deadlines or other
// context values. WithClientTrace would compose callbacks rather than hide them.
type privateTraceContext struct{ context.Context }

func (c privateTraceContext) Value(key any) any {
	value := c.Context.Value(key)
	if _, ok := value.(*httptrace.ClientTrace); ok {
		return nil
	}
	return value
}

type timeoutError struct{ message string }

func (e *timeoutError) Error() string { return e.message }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

var _ net.Error = (*timeoutError)(nil)

func safeError(stage string, err error) error {
	message := "bedrock runtime: " + stage + " failed"
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%s: %w", message, sentinel)
		}
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return &timeoutError{message: message}
	}
	// Do not retain the original error even as an unwrap target: callers could
	// otherwise recover SDK response bodies, JWTs, or signed requests from it.
	return errors.New(message)
}
