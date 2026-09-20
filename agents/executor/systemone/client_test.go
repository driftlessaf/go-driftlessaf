/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/google/go-cmp/cmp"
)

// fastRetry keeps retry tests off the wall clock.
func fastRetry(maxRetries int) retry.RetryConfig {
	return retry.RetryConfig{MaxRetries: maxRetries, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
}

func sampleRequest() Request {
	return Request{
		Model: ModelJevLatest,
		State: "The install script downloads and executes a remote binary.",
		Questions: map[string]Question{
			"exfil": Noul{Instructions: "Does the state describe data exfiltration?"},
			"kind": Choice{
				Instructions: "Classify the behavior.",
				Options:      map[string]Content{"benign": nil, "suspicious": "warrants review", "malicious": "clearly hostile"},
			},
			"severity": Score{
				Instructions: "Rate the severity.",
				Levels:       []Content{"none", "low", "high"},
			},
		},
	}
}

const sampleAnswers = `{
	"exfil": {"type": "noul", "noul": 0.91},
	"kind": {"type": "choice", "choice": "malicious", "probabilities": {"benign": 0.02, "suspicious": 0.18, "malicious": 0.80}, "confidence": 0.77},
	"severity": {"type": "score", "score": 1.6, "legend": {"0": "none", "1": "low", "2": "high"}, "probabilities": {"0": 0.1, "1": 0.2, "2": 0.7}, "confidence": 0.65}
}`

func sampleBody(answers string) string {
	return `{"model": "jev-1.13.0", "answers": ` + answers + `, "usage": {"input_tokens": 120, "output_tokens": 3}}`
}

// newServer returns a client wired to an httptest server driven by handler.
func newServer(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	opts = append([]Option{WithEndpoint(srv.URL), WithHTTPClient(srv.Client()), WithRetryConfig(fastRetry(0))}, opts...)
	client, err := NewClient("sk-test-"+t.Name(), opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestAskRoundTrip(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	var gotHeader http.Header
	client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		var err error
		gotBody, err = readAll(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleBody(sampleAnswers)))
	})

	resp, err := client.Ask(t.Context(), sampleRequest())
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if got, want := gotHeader.Get("Authorization"), "Bearer sk-test-"+t.Name(); got != want {
		t.Errorf("Authorization: got = %q, want = %q", got, want)
	}
	if got, want := gotHeader.Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type: got = %q, want = %q", got, want)
	}

	var wire map[string]any
	if err := json.Unmarshal(gotBody, &wire); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	wantWire := map[string]any{
		"model": "jev-latest",
		"state": "The install script downloads and executes a remote binary.",
		"questions": map[string]any{
			"exfil": map[string]any{"type": "noul", "instructions": "Does the state describe data exfiltration?"},
			"kind": map[string]any{
				"type":         "choice",
				"instructions": "Classify the behavior.",
				"criteria":     map[string]any{"benign": nil, "suspicious": "warrants review", "malicious": "clearly hostile"},
			},
			"severity": map[string]any{
				"type":         "score",
				"instructions": "Rate the severity.",
				"criteria":     []any{"none", "low", "high"},
			},
		},
	}
	if diff := cmp.Diff(wantWire, wire); diff != "" {
		t.Errorf("request wire body (-want, +got):\n%s", diff)
	}

	want := &Response{
		Model: "jev-1.13.0",
		Usage: Usage{InputTokens: 120, OutputTokens: 3},
		Answers: map[string]Answer{
			"exfil": NoulAnswer{Probability: 0.91},
			"kind": ChoiceAnswer{
				Choice:        "malicious",
				Probabilities: map[string]float64{"benign": 0.02, "suspicious": 0.18, "malicious": 0.80},
				Confidence:    0.77,
			},
			"severity": ScoreAnswer{
				Score:         1.6,
				Legend:        map[string]string{"0": "none", "1": "low", "2": "high"},
				Probabilities: map[string]float64{"0": 0.1, "1": 0.2, "2": 0.7},
				Confidence:    0.65,
			},
		},
	}
	if diff := cmp.Diff(want, resp); diff != "" {
		t.Errorf("response (-want, +got):\n%s", diff)
	}
}

// Pointer-valued questions satisfy Question through method-set promotion; the
// request and the response decoder must treat them exactly like values.
func TestAskAcceptsPointerQuestions(t *testing.T) {
	t.Parallel()
	client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleBody(sampleAnswers)))
	})
	req := sampleRequest()
	req.Questions = map[string]Question{
		"exfil":    &Noul{Instructions: "Does the state describe data exfiltration?"},
		"kind":     &Choice{Instructions: "Classify the behavior.", Options: req.Questions["kind"].(Choice).Options},
		"severity": &Score{Instructions: "Rate the severity.", Levels: req.Questions["severity"].(Score).Levels},
	}
	resp, err := client.Ask(t.Context(), req)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	for id, want := range map[string]string{"exfil": "noul", "kind": "choice", "severity": "score"} {
		if got := resp.Answers[id].Kind(); got != want {
			t.Errorf("answer %q kind: got = %q, want = %q", id, got, want)
		}
	}
}

func TestAskRejectsInvalidRequestBeforeSending(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := newServer(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })

	tests := []struct {
		name string
		req  Request
	}{
		{name: "no model", req: Request{State: "s", Questions: map[string]Question{"q": Noul{Instructions: "i"}}}},
		{name: "nil state", req: Request{Model: ModelJevLatest, Questions: map[string]Question{"q": Noul{Instructions: "i"}}}},
		{name: "no questions", req: Request{Model: ModelJevLatest, State: "s"}},
		{name: "empty id", req: Request{Model: ModelJevLatest, State: "s", Questions: map[string]Question{"": Noul{Instructions: "i"}}}},
		{name: "nil question", req: Request{Model: ModelJevLatest, State: "s", Questions: map[string]Question{"q": nil}}},
		{name: "nil pointer question", req: Request{Model: ModelJevLatest, State: "s", Questions: map[string]Question{"q": (*Noul)(nil)}}},
		{name: "one option", req: Request{Model: ModelJevLatest, State: "s", Questions: map[string]Question{"q": Choice{Instructions: "i", Options: map[string]Content{"a": nil}}}}},
		{name: "one level", req: Request{Model: ModelJevLatest, State: "s", Questions: map[string]Question{"q": Score{Instructions: "i", Levels: []Content{"a"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := client.Ask(t.Context(), tt.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("Ask error: got = %v, want ErrInvalidRequest", err)
			}
		})
	}
	t.Cleanup(func() {
		if n := calls.Load(); n != 0 {
			t.Errorf("server calls: got = %d, want = 0", n)
		}
	})
}

func TestAskRetriesTransientStatusesThenSucceeds(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	statuses := []int{http.StatusTooManyRequests, StatusOverloaded}
	client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1)) - 1
		if n < len(statuses) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(statuses[n])
			return
		}
		_, _ = w.Write([]byte(sampleBody(sampleAnswers)))
	}, WithRetryConfig(fastRetry(len(statuses))))

	if _, err := client.Ask(t.Context(), sampleRequest()); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got, want := calls.Load(), int32(len(statuses)+1); got != want {
		t.Errorf("attempts: got = %d, want = %d", got, want)
	}
}

func TestAskSurfacesLastRetryableError(t *testing.T) {
	t.Parallel()
	client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(StatusOverloaded)
		_, _ = w.Write([]byte("{\"error\":\"overloaded\n\x1b[31m\"}"))
	}, WithRetryConfig(fastRetry(1)))

	_, err := client.Ask(t.Context(), sampleRequest())
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		t.Fatalf("Ask error: got = %v, want *APIError", err)
	}
	want := &APIError{StatusCode: StatusOverloaded, Body: `{"error":"overloaded [31m"}`, RetryAfter: 7 * time.Second}
	if diff := cmp.Diff(want, apiErr); diff != "" {
		t.Errorf("APIError (-want, +got):\n%s", diff)
	}
	if !IsRetryable(err) {
		t.Errorf("IsRetryable: got = false, want = true")
	}
	if strings.Contains(err.Error(), "sk-test-") {
		t.Errorf("error message leaks the API key: %q", err.Error())
	}
}

func TestAskDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"questions.kind.criteria: too few options"}`))
	}, WithRetryConfig(fastRetry(3)))

	_, err := client.Ask(t.Context(), sampleRequest())
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		t.Fatalf("Ask error: got = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode: got = %d, want = %d", apiErr.StatusCode, http.StatusUnprocessableEntity)
	}
	if IsRetryable(err) {
		t.Errorf("IsRetryable: got = true, want = false")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts: got = %d, want = 1", got)
	}
	if got, want := err.Error(), "system one: HTTP 422 Unprocessable Entity: {\"detail\":\"questions.kind.criteria: too few options\"}"; got != want {
		t.Errorf("Error(): got = %q, want = %q", got, want)
	}
}

func TestAskValidatesAnswersAgainstQuestions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		answers string
	}{
		{name: "missing answer", answers: `{"exfil": {"type": "noul", "noul": 0.5}}`},
		{name: "extra answer", answers: strings.TrimSuffix(sampleAnswers, "}") + `, "bonus": {"type": "noul", "noul": 0.5}}`},
		{name: "kind mismatch", answers: strings.Replace(sampleAnswers, `"type": "noul", "noul": 0.91`, `"type": "choice", "choice": "x", "confidence": 1`, 1)},
		{name: "noul above one", answers: strings.Replace(sampleAnswers, `"noul": 0.91`, `"noul": 1.5`, 1)},
		{name: "noul missing value", answers: strings.Replace(sampleAnswers, `"noul": 0.91`, `"nope": 0.91`, 1)},
		{name: "undeclared choice", answers: strings.Replace(sampleAnswers, `"choice": "malicious"`, `"choice": "greyware"`, 1)},
		{name: "undeclared option probability", answers: strings.Replace(sampleAnswers, `"benign": 0.02`, `"greyware": 0.02`, 1)},
		{name: "choice probabilities absent", answers: strings.Replace(sampleAnswers, `"probabilities": {"benign": 0.02, "suspicious": 0.18, "malicious": 0.80}, `, ``, 1)},
		{name: "choice probabilities empty", answers: strings.Replace(sampleAnswers, `{"benign": 0.02, "suspicious": 0.18, "malicious": 0.80}`, `{}`, 1)},
		{name: "choice probabilities missing an option", answers: strings.Replace(sampleAnswers, `"benign": 0.02, `, ``, 1)},
		{name: "choice probabilities do not sum to one", answers: strings.Replace(sampleAnswers, `"malicious": 0.80`, `"malicious": 0.50`, 1)},
		{name: "option probability negative", answers: strings.Replace(sampleAnswers, `"benign": 0.02`, `"benign": -0.02`, 1)},
		{name: "choice confidence missing", answers: strings.Replace(sampleAnswers, `"confidence": 0.77`, `"confidence": null`, 1)},
		{name: "score above rubric", answers: strings.Replace(sampleAnswers, `"score": 1.6`, `"score": 2.4`, 1)},
		{name: "score missing", answers: strings.Replace(sampleAnswers, `"score": 1.6`, `"score": null`, 1)},
		{name: "score level probability above one", answers: strings.Replace(sampleAnswers, `"2": 0.7`, `"2": 1.7`, 1)},
		{name: "score probabilities absent", answers: strings.Replace(sampleAnswers, `"probabilities": {"0": 0.1, "1": 0.2, "2": 0.7}, `, ``, 1)},
		{name: "score probability for undeclared level", answers: strings.Replace(sampleAnswers, `"2": 0.7`, `"3": 0.7`, 1)},
		{name: "score probability keyed by description", answers: strings.Replace(sampleAnswers, `"2": 0.7`, `"high": 0.7`, 1)},
		{name: "score probability key not canonical", answers: strings.Replace(sampleAnswers, `"2": 0.7`, `"02": 0.7`, 1)},
		{name: "score probabilities missing a level", answers: strings.Replace(sampleAnswers, `"0": 0.1, `, ``, 1)},
		{name: "score probabilities do not sum to one", answers: strings.Replace(sampleAnswers, `"2": 0.7`, `"2": 0.4`, 1)},
		{name: "score legend absent", answers: strings.Replace(sampleAnswers, `"legend": {"0": "none", "1": "low", "2": "high"}, `, ``, 1)},
		{name: "score legend missing a level", answers: strings.Replace(sampleAnswers, `"1": "low", `, ``, 1)},
		{name: "score legend keyed off the rubric", answers: strings.Replace(sampleAnswers, `"2": "high"`, `"3": "high"`, 1)},
		{name: "not json", answers: `nope`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(sampleBody(tt.answers)))
			})
			_, err := client.Ask(t.Context(), sampleRequest())
			if !errors.Is(err, ErrResponseValidation) {
				t.Errorf("Ask error: got = %v, want ErrResponseValidation", err)
			}
			if IsRetryable(err) {
				t.Errorf("IsRetryable: got = true, want = false")
			}
		})
	}
}

func TestAskCapsResponseSize(t *testing.T) {
	t.Parallel()
	client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleBody(sampleAnswers)))
	}, WithMaxResponseBytes(64))

	_, err := client.Ask(t.Context(), sampleRequest())
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("Ask error: got = %v, want ErrResponseTooLarge", err)
	}
}

// An HTTP client timeout on one attempt is a transient failure: it is
// retried while the caller's context is still live, even though net/http
// reports it as an error matching context.DeadlineExceeded.
func TestAskRetriesHTTPTimeoutWhileCallerContextLive(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	// release lets the held handler return once the test has its answer; a
	// handler that never returns would block the server's Close.
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Hold the first attempt until the client gives up on it.
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		_, _ = w.Write([]byte(sampleBody(sampleAnswers)))
	}))
	t.Cleanup(srv.Close)
	httpClient := srv.Client()
	httpClient.Timeout = 200 * time.Millisecond
	client, err := NewClient("k", WithEndpoint(srv.URL), WithHTTPClient(httpClient), WithRetryConfig(fastRetry(1)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.Ask(t.Context(), sampleRequest()); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts: got = %d, want = 2", got)
	}
}

// An expired caller deadline also surfaces as a net/http timeout, but it is
// the caller's budget, so no further attempt is made.
func TestAskDoesNotRetryAfterCallerDeadline(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	defer close(release)
	client := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}, WithRetryConfig(fastRetry(3)))

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := client.Ask(ctx, sampleRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ask error: got = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts: got = %d, want = 1", got)
	}
}

func TestAskStopsOnContextCancellation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}, WithRetryConfig(retry.RetryConfig{MaxRetries: 5, BaseBackoff: time.Hour, MaxBackoff: time.Hour}))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.Ask(ctx, sampleRequest())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Ask error: got = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got > 1 {
		t.Errorf("attempts after cancellation: got = %d, want <= 1", got)
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "429", err: &APIError{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "408", err: &APIError{StatusCode: http.StatusRequestTimeout}, want: true},
		{name: "500", err: &APIError{StatusCode: http.StatusInternalServerError}, want: true},
		{name: "529", err: &APIError{StatusCode: StatusOverloaded}, want: true},
		{name: "401", err: &APIError{StatusCode: http.StatusUnauthorized}, want: false},
		{name: "422", err: &APIError{StatusCode: http.StatusUnprocessableEntity}, want: false},
		{name: "wrapped 503", err: errors.Join(errors.New("outer"), &APIError{StatusCode: http.StatusServiceUnavailable}), want: true},
		{name: "validation", err: validationError("bad"), want: false},
		{name: "too large", err: ErrResponseTooLarge, want: false},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: false},
		{name: "net timeout", err: &net.OpError{Op: "dial", Err: timeoutErr{}}, want: true},
		{name: "http client timeout", err: &url.Error{Op: "Post", Err: timeoutErr{}}, want: true},
		{name: "http deadline exceeded mid-request", err: &url.Error{Op: "Post", Err: context.DeadlineExceeded}, want: true},
		{name: "http request canceled", err: &url.Error{Op: "Post", Err: context.Canceled}, want: false},
		{name: "http connection refused", err: &url.Error{Op: "Post", Err: errors.New("connection refused")}, want: false},
		{name: "net non-timeout", err: &net.OpError{Op: "dial", Err: errors.New("refused")}, want: false},
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

// timeoutErr is a net.Error whose Timeout reports true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestNewClientOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		apiKey  string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults", apiKey: "k"},
		{name: "empty key", apiKey: "", wantErr: true},
		{name: "relative endpoint", apiKey: "k", opts: []Option{WithEndpoint("/v1/systemone")}, wantErr: true},
		{name: "ftp endpoint", apiKey: "k", opts: []Option{WithEndpoint("ftp://x/y")}, wantErr: true},
		{name: "http endpoint", apiKey: "k", opts: []Option{WithEndpoint("http://127.0.0.1:1/x")}},
		{name: "nil http client", apiKey: "k", opts: []Option{WithHTTPClient(nil)}, wantErr: true},
		{name: "nil metrics", apiKey: "k", opts: []Option{WithMetrics(nil)}, wantErr: true},
		{name: "negative retries", apiKey: "k", opts: []Option{WithRetryConfig(retry.RetryConfig{MaxRetries: -1})}, wantErr: true},
		{name: "zero size cap", apiKey: "k", opts: []Option{WithMaxResponseBytes(0)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewClient(tt.apiKey, tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewClient error: got = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func systemOneRegistry(t *testing.T) *modelrouter.Registry {
	t.Helper()
	registry, err := modelrouter.NewRegistry(
		modelrouter.Route{
			Selection:       modelrouter.Selection{Provider: modelrouter.ProviderTypeSafe, LogicalModel: ModelJevLatest},
			Protocol:        modelrouter.ProtocolTypeSafeSystemOne,
			ProviderModelID: "jev-1.13.0",
			Attribution:     modelrouter.Attribution{ProviderName: "typesafe", LegacySystem: "typesafe"},
		},
		modelrouter.Route{
			Selection:       modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: "claude-sonnet-5"},
			Protocol:        modelrouter.ProtocolAnthropicMessages,
			ProviderModelID: "claude-sonnet-5",
			Attribution:     modelrouter.Attribution{ProviderName: "anthropic", LegacySystem: "anthropic"},
		},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return registry
}

func TestWithRouteSuppliesModel(t *testing.T) {
	t.Parallel()
	plan, err := systemOneRegistry(t).Resolve(modelrouter.Selection{Provider: modelrouter.ProviderTypeSafe, LogicalModel: ModelJevLatest})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var gotModels []string
	client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		var wire struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotModels = append(gotModels, wire.Model)
		_, _ = w.Write([]byte(sampleBody(sampleAnswers)))
	}, WithRoute(plan))

	unset := sampleRequest()
	unset.Model = ""
	if _, err := client.Ask(t.Context(), unset); err != nil {
		t.Fatalf("Ask without model: %v", err)
	}
	explicit := sampleRequest()
	explicit.Model = ModelJevPreview
	if _, err := client.Ask(t.Context(), explicit); err != nil {
		t.Fatalf("Ask with model: %v", err)
	}
	if diff := cmp.Diff([]string{"jev-1.13.0", ModelJevPreview}, gotModels); diff != "" {
		t.Errorf("models sent (-want, +got):\n%s", diff)
	}
}

func TestWithRouteRejectsOtherProtocols(t *testing.T) {
	t.Parallel()
	plan, err := systemOneRegistry(t).Resolve(modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := NewClient("k", WithRoute(plan)); err == nil || !strings.Contains(err.Error(), `protocol "anthropic-messages" is not "typesafe-system-one"`) {
		t.Errorf("NewClient error: got = %v, want protocol mismatch", err)
	}
	if _, err := NewClient("k", WithRoute(modelrouter.Plan{})); !errors.Is(err, modelrouter.ErrInvalidPlan) {
		t.Errorf("NewClient error with zero plan: got = %v, want ErrInvalidPlan", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  time.Duration
	}{
		{value: "", want: 0},
		{value: "3", want: 3 * time.Second},
		{value: "0", want: 0},
		{value: "-1", want: 0},
		{value: "Wed, 21 Oct 2015 07:28:00 GMT", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(tt.value); got != tt.want {
				t.Errorf("parseRetryAfter(%q): got = %v, want = %v", tt.value, got, tt.want)
			}
		})
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf strings.Builder
	if _, err := io.Copy(&buf, r.Body); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
