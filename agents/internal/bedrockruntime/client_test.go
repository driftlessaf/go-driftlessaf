/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package bedrockruntime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"chainguard.dev/driftlessaf/agents/awsauth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/go-cmp/cmp"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var _ http.RoundTripper = roundTripFunc(nil)

func testCredentials() aws.Credentials {
	return aws.Credentials{AccessKeyID: rand.Text(), SecretAccessKey: rand.Text(), SessionToken: rand.Text()}
}

func testClient(t *testing.T, credentials aws.Credentials, rt http.RoundTripper) *client {
	t.Helper()
	client, err := newClient(aws.Config{
		Region: "us-west-2",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return credentials, nil
		}),
	}, rt, func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("newClient: got = %v, want nil", err)
	}
	return client
}

func testRequest(t *testing.T, ctx context.Context, target, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext: got = %v, want nil", err)
	}
	request.Header.Set("Content-Type", "application/json")
	return request
}

func okResponse(r *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: r, Body: http.NoBody}
}

func TestSignedRequestIsolation(t *testing.T) {
	t.Parallel()
	credentials := testCredentials()
	payload := rand.Text()
	var calls atomic.Int32
	client := testClient(t, credentials, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential="+credentials.AccessKeyID+"/20260901/us-west-2/bedrock/aws4_request,") {
			t.Error("Authorization: got an unexpected signature scope, want regional bedrock SigV4")
		}
		if got := r.Header.Get("X-Amz-Security-Token"); got != credentials.SessionToken {
			t.Error("session token: got a different value, want provider token")
		}
		if got := r.Header.Get("X-Amz-Date"); got != "20260901T000000Z" {
			t.Errorf("signing date: got = %q, want fixed clock", got)
		}
		for _, key := range []string{"X-Api-Key", "Cookie", "Proxy-Authorization"} {
			if r.Header.Get(key) != "" {
				t.Errorf("%s: got a credential header, want absent", key)
			}
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type: got = %q, want application/json", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != payload {
			t.Errorf("body: got = %q, %v, want = %q, nil", body, err, payload)
		}
		if r.ContentLength != int64(len(payload)) {
			t.Errorf("ContentLength: got = %d, want = %d", r.ContentLength, len(payload))
		}
		if httptrace.ContextClientTrace(r.Context()) != nil {
			t.Error("HTTP trace: got caller hook, want suppressed")
		}
		response := okResponse(r)
		response.Header.Set("x-amzn-requestid", "request-id")
		response.Header.Set("Authorization", r.Header.Get("Authorization"))
		return response, nil
	}))
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{WroteHeaderField: func(string, []string) {
		t.Error("HTTP trace callback ran inside the signing boundary")
	}})
	request := testRequest(t, ctx, client.Endpoint()+"/openai/v1/responses", payload)
	// Noncanonical names must not bypass credential stripping.
	request.Header["authorization"] = []string{"Bearer " + rand.Text()}
	request.Header["x-api-key"] = []string{rand.Text()}
	request.Header.Set("Cookie", rand.Text())
	request.Header.Set("Proxy-Authorization", rand.Text())
	request.Header.Set("X-Amz-Security-Token", rand.Text())
	request.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	wantHeaders := request.Header.Clone()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: got = %v, want nil", err)
	}
	defer response.Body.Close()
	if diff := cmp.Diff(wantHeaders, request.Header); diff != "" {
		t.Errorf("request headers (-want, +got): %s", diff)
	}
	if response.Request == request || response.Request.Header.Get("Authorization") != "" || response.Header.Get("Authorization") != "" || response.Request.Body != nil || response.Request.GetBody != nil {
		t.Error("response: got signed or replayable request data, want sanitized metadata")
	}
	if got := response.Header.Get("x-amzn-requestid"); got != "request-id" {
		t.Errorf("AWS request ID: got = %q, want request-id", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls: got = %d, want 1", got)
	}
}

func TestRejectsUnsafeDestinations(t *testing.T) {
	t.Parallel()
	for _, target := range []string{
		"http://bedrock-runtime.us-west-2.amazonaws.com/openai/v1/responses",
		"https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1/responses",
		"https://bedrock-runtime.us-west-2.amazonaws.com.attacker.example/",
		"https://bedrock-runtime.us-west-2.amazonaws.com:443/",
		"https://user:secret@bedrock-runtime.us-west-2.amazonaws.com/",
		"https://bedrock-runtime.us-west-2.amazonaws.com/?X-Amz-Credential=secret",
		"https://bedrock-runtime.us-west-2.amazonaws.com/#secret",
	} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			client := testClient(t, testCredentials(), roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("RoundTrip: got call, want rejected destination")
				return nil, nil
			}))
			response, err := client.Do(testRequest(t, t.Context(), target, "{}"))
			if response != nil {
				response.Body.Close()
			}
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Errorf("Do: got = %v, want sanitized destination rejection", err)
			}
		})
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Host = "attacker.example" },
		func(r *http.Request) { r.URL.Opaque = "//attacker.example/" },
		func(r *http.Request) { r.RequestURI = "/untrusted" },
		func(r *http.Request) { r.URL = nil },
	} {
		client := testClient(t, testCredentials(), roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("RoundTrip: got call, want rejected override")
			return nil, nil
		}))
		request := testRequest(t, t.Context(), client.Endpoint(), "{}")
		mutate(request)
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Error("Do: got nil, want override rejection")
		}
	}
}

func TestTLSRedirectAndStreaming(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTemporaryRedirect, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			release := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("wire authorization: got unsigned request, want SigV4")
				}
				w.Header().Set("Location", "https://attacker.example/")
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer close(release)
			transport := server.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig.ServerName = server.Certificate().DNSNames[0]
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			defer transport.CloseIdleConnections()
			client := testClient(t, testCredentials(), transport)
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			response, err := client.Do(testRequest(t, ctx, client.Endpoint()+"/openai/v1/chat/completions", "{}"))
			if status == http.StatusTemporaryRedirect {
				if err == nil || response != nil {
					t.Fatalf("redirect: got = %v, %v, want rejection", response, err)
				}
			} else {
				if err != nil {
					t.Fatalf("Do before response EOF: got = %v, want streaming response", err)
				}
				defer response.Body.Close()
				if response.StatusCode != status {
					t.Errorf("status: got = %d, want = %d", response.StatusCode, status)
				}
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("requests: got = %d, want 1 without redirect or retry", got)
			}
		})
	}
}

type failingBody struct{ closed bool }

var _ io.ReadCloser = (*failingBody)(nil)

func (*failingBody) Read([]byte) (int, error) { return 0, errors.New("secret body failure") }
func (b *failingBody) Close() error           { b.closed = true; return nil }

func TestBodyLimitsAndFailures(t *testing.T) {
	t.Parallel()
	client := testClient(t, testCredentials(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("RoundTrip: got call, want body rejection")
		return nil, nil
	}))
	for _, knownLength := range []bool{true, false} {
		request := testRequest(t, t.Context(), client.Endpoint(), strings.Repeat("x", maxRequestBytes+1))
		if !knownLength {
			request.ContentLength = -1
		}
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Error("oversized body: got nil, want rejection")
		}
	}
	body := &failingBody{}
	request := testRequest(t, t.Context(), client.Endpoint(), "")
	request.Body = body
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || strings.Contains(err.Error(), "secret") || !body.closed {
		t.Errorf("body failure: got = %v, closed = %v, want sanitized error and closed body", err, body.closed)
	}
}

func TestRequestAtBodyLimit(t *testing.T) {
	for _, tt := range []struct {
		name        string
		knownLength bool
	}{
		{name: "known content length", knownLength: true},
		{name: "unknown content length"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			client := testClient(t, testCredentials(), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if got := r.ContentLength; got != maxRequestBytes {
					t.Errorf("ContentLength: got = %d, want = %d", got, maxRequestBytes)
				}
				if got, err := io.Copy(io.Discard, r.Body); err != nil || got != maxRequestBytes {
					t.Errorf("body bytes: got = %d, %v, want = %d, nil", got, err, maxRequestBytes)
				}
				return okResponse(r), nil
			}))
			request := testRequest(t, t.Context(), client.Endpoint(), strings.Repeat("x", maxRequestBytes))
			if !tt.knownLength {
				request.ContentLength = -1
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("Do at body limit: got = %v, want nil", err)
			}
			defer response.Body.Close()
			if calls != 1 {
				t.Errorf("RoundTrip calls: got = %d, want 1", calls)
			}
		})
	}
}

func TestSafeErrors(t *testing.T) {
	t.Parallel()
	for _, original := range []error{
		errors.New("secret token"),
		&url.Error{Op: "Post", URL: "https://secret@host", Err: errors.New("secret")},
		fmt.Errorf("secret: %w", context.Canceled),
		fmt.Errorf("secret: %w", context.DeadlineExceeded),
		&net.DNSError{Err: "secret", Name: "secret", IsTimeout: true},
	} {
		got := safeError("operation", original)
		if strings.Contains(got.Error(), "secret") {
			t.Errorf("safe error: got = %v, want no secret", got)
		}
		for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
			if errors.Is(got, sentinel) != errors.Is(original, sentinel) {
				t.Errorf("errors.Is(%v): got classification mismatch", sentinel)
			}
		}
		if netErr, ok := errors.AsType[net.Error](original); ok && netErr.Timeout() {
			if clean, ok := errors.AsType[net.Error](got); !ok || !clean.Timeout() {
				t.Error("timeout: got lost classification, want net.Error timeout")
			}
		}
		if errors.Is(got, original) {
			t.Error("error chain: got raw error, want no raw cause")
		}
	}
}

func TestCancellationAndConcurrentCalls(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := testClient(t, testCredentials(), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse(r), nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response, err := client.Do(testRequest(t, ctx, client.Endpoint(), ""))
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation: got = %v, want context.Canceled", err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			response, err := client.Do(testRequest(t, t.Context(), client.Endpoint(), rand.Text()))
			if err != nil {
				t.Errorf("concurrent Do: got = %v, want nil", err)
				return
			}
			response.Body.Close()
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 16 {
		t.Errorf("calls: got = %d, want 16", got)
	}
}

func TestCancellationDuringBodyRead(t *testing.T) {
	t.Parallel()
	client := testClient(t, testCredentials(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("RoundTrip: got call, want cancellation before signing")
		return nil, errors.New("unexpected call")
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	request := testRequest(t, ctx, client.Endpoint(), "")
	request.Body = reader
	done := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	// Completing this write proves Do started reading before cancellation.
	if _, err := writer.Write([]byte("{")); err != nil {
		t.Fatalf("Write: got = %v, want nil", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Do: got = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		reader.Close()
		t.Fatal("Do: got blocked body read, want cancellation")
	}
}

func TestCredentialAndTransportFailures(t *testing.T) {
	t.Parallel()
	secret := rand.Text()
	for _, stage := range []string{"refresh", "missing session token", "transport"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			credentials := testCredentials()
			if stage == "missing session token" {
				credentials.SessionToken = ""
			}
			client := testClient(t, credentials, roundTripFunc(func(*http.Request) (*http.Response, error) {
				if stage != "transport" {
					t.Error("RoundTrip: got call, want credential rejection")
				}
				return nil, errors.New(secret)
			}))
			if stage == "refresh" {
				client.credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{}, errors.New(secret)
				})
			}
			response, err := client.Do(testRequest(t, t.Context(), client.Endpoint(), "{}"))
			if response != nil {
				response.Body.Close()
			}
			if response != nil || err == nil || strings.Contains(err.Error(), secret) || errors.Unwrap(err) != nil {
				t.Error("Do: got response or unsanitized failure, want isolated error without raw cause")
			}
		})
	}
}

func TestNewRejectsUnsupportedRegions(t *testing.T) {
	t.Parallel()
	for _, region := range []string{"", "US-WEST-2", "us-west-2.attacker.example", "cn-north-1", "us-gov-west-1", "us-iso-east-1", "eu-isoe-west-1"} {
		client, err := New(t.Context(), awsauth.Config{Region: region})
		if err == nil || client != nil {
			t.Errorf("New(%q): got = %v, %v, want region rejection before credential discovery", region, client, err)
		}
	}
}

func TestNewSanitizesCredentialDiscoveryFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "missing-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "missing-credentials"))
	profile := rand.Text()
	client, err := New(t.Context(), awsauth.Config{Region: "us-west-2", Profile: profile})
	if client != nil || err == nil || strings.Contains(err.Error(), profile) || errors.Unwrap(err) != nil {
		t.Error("New: got client or unsanitized discovery error, want isolated error without profile or raw cause")
	}
}

func TestCredentialExpiryRefreshesRequestSigning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Exercise the real cache and signer. The provider supplies expiring
		// credentials as STS would; only issuance and the wire exchange are doubles.
		issued := []aws.Credentials{testCredentials(), testCredentials(), testCredentials()}
		var retrievals, issuedCount int
		var rejectRefresh bool
		cache := aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			retrievals++
			if rejectRefresh {
				return aws.Credentials{}, errors.New("identity exchange unavailable")
			}
			if issuedCount >= len(issued) {
				return aws.Credentials{}, errors.New("unexpected extra credential exchange")
			}
			credentials := issued[issuedCount]
			issuedCount++
			credentials.CanExpire = true
			credentials.Expires = time.Now().Add(time.Hour)
			return credentials, nil
		}), func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = time.Minute })
		var wireCalls int
		var expected aws.Credentials
		client, err := newClient(aws.Config{Region: "us-east-1", Credentials: cache}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			wireCalls++
			if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential="+expected.AccessKeyID+"/") || r.Header.Get("X-Amz-Security-Token") != expected.SessionToken {
				t.Error("signed request: got stale signing identity, want current credentials")
			}
			return okResponse(r), nil
		}), time.Now)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range []struct {
			name           string
			advance        time.Duration
			credential     int
			reject         bool
			wantRetrievals int
			wantWireCalls  int
		}{
			{name: "initial request", credential: 0, wantRetrievals: 1, wantWireCalls: 1},
			{name: "cached request", credential: 0, wantRetrievals: 1, wantWireCalls: 2},
			{name: "before refresh window", advance: 58 * time.Minute, credential: 0, wantRetrievals: 1, wantWireCalls: 3},
			{name: "inside refresh window", advance: 61 * time.Second, credential: 1, wantRetrievals: 2, wantWireCalls: 4},
			{name: "reuse refreshed credentials", credential: 1, wantRetrievals: 2, wantWireCalls: 5},
			{name: "refresh failure after idle hour", advance: 65 * time.Minute, reject: true, wantRetrievals: 3, wantWireCalls: 5},
			{name: "recover on next request", credential: 2, wantRetrievals: 4, wantWireCalls: 6},
		} {
			// Inside synctest this advances virtual time, including the SDK's
			// expiration clock. No cache invalidation or client reconstruction.
			time.Sleep(step.advance)
			rejectRefresh = step.reject
			expected = issued[step.credential]
			response, err := client.Do(testRequest(t, t.Context(), client.Endpoint()+"/anthropic/v1/messages", "{}"))
			if response != nil {
				response.Body.Close()
			}
			if step.reject {
				if err == nil || !strings.Contains(err.Error(), "credential refresh") || response != nil {
					t.Fatalf("%s: got response = %v, error = %v, want refresh failure without response", step.name, response, err)
				}
			} else if err != nil {
				t.Fatalf("%s: Do() error = %v, want nil", step.name, err)
			}
			if retrievals != step.wantRetrievals || wireCalls != step.wantWireCalls {
				t.Fatalf("%s: retrievals, wire calls = (%d, %d), want (%d, %d)", step.name, retrievals, wireCalls, step.wantRetrievals, step.wantWireCalls)
			}
		}
	})
}

func TestWebIdentityRefresh(t *testing.T) {
	// Use the real awsauth loader and AWS SDK STS provider. Only STS's network
	// endpoint and the final Bedrock HTTP exchange are replaced with test servers.
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_BEARER_TOKEN_BEDROCK", "ANTHROPIC_AWS_API_KEY"} {
		t.Setenv(name, "")
	}
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	tokenFile := filepath.Join(dir, "identity")
	firstToken, nextToken := rand.Text(), rand.Text()
	if err := os.WriteFile(tokenFile, []byte(firstToken), 0o600); err != nil {
		t.Fatalf("WriteFile: got = %v, want nil", err)
	}
	first, next := testCredentials(), testCredentials()
	var stsCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: got = %v, want nil", err)
		}
		credentials, token := first, firstToken
		if stsCalls.Add(1) > 1 {
			credentials, token = next, nextToken
		}
		if r.Form.Get("Action") != "AssumeRoleWithWebIdentity" || r.Form.Get("WebIdentityToken") != token || r.Form.Get("RoleArn") != "arn:aws:iam::123456789012:role/eval" {
			t.Error("STS request: got unexpected identity, want originally selected provider and current token file")
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, credentials.AccessKeyID, credentials.SecretAccessKey, credentials.SessionToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	t.Setenv("AWS_ENDPOINT_URL_STS", server.URL)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/eval")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{WroteHeaderField: func(string, []string) {
		t.Error("HTTP trace: got credential discovery callback, want suppressed")
	}})
	transport, err := New(ctx, awsauth.Config{Region: "us-west-2"})
	if err != nil {
		t.Fatalf("New: got = %v, want nil", err)
	}
	client := transport.(*client)
	defer client.CloseIdleConnections()
	expected := first
	client.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.Header.Get("Authorization"), expected.AccessKeyID) || r.Header.Get("X-Amz-Security-Token") != expected.SessionToken {
			t.Error("signed request: got stale credentials, want refreshed credentials")
		}
		return okResponse(r), nil
	})
	for range 2 {
		response, err := client.Do(testRequest(t, t.Context(), client.Endpoint(), "{}"))
		if err != nil {
			t.Fatalf("Do: got = %v, want nil", err)
		}
		response.Body.Close()
		if err := os.WriteFile(tokenFile, []byte(nextToken), 0o600); err != nil {
			t.Fatalf("WriteFile: got = %v, want nil", err)
		}
		expected = next
		client.credentials.(*aws.CredentialsCache).Invalidate()
		// Refresh must not switch to a different chain after construction.
		t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/other")
	}
	if got := stsCalls.Load(); got != 2 {
		t.Errorf("STS calls: got = %d, want 2", got)
	}
}
