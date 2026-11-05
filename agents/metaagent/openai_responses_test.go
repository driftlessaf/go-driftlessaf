/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/openaiauth"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go/v3/responses"
)

func openAIDirectRoute() modelrouter.Route {
	// A protocol fixture, not a claim that an organization can use the model.
	return modelrouter.Route{
		Selection:       modelrouter.Selection{Provider: modelrouter.ProviderOpenAI, LogicalModel: "gpt-5.5-cyber-preview"},
		Protocol:        modelrouter.ProtocolOpenAIResponses,
		ProviderModelID: "gpt-5.5-cyber-preview",
		Attribution:     modelrouter.Attribution{ProviderName: agenttrace.SystemOpenAI, LegacySystem: agenttrace.SystemOpenAI},
		Capabilities: modelrouter.Capabilities{
			Efforts:     []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh},
			ToolCalling: true, TerminalSubmission: true, MaximumOutputTokens: true,
		},
	}
}

// fakeOpenAI serves OpenAI's token exchange and Responses API, which live on
// two fixed HTTPS hosts. Its transport sends both to one local server and
// records which host each request was addressed to.
type fakeOpenAI struct {
	server      *httptest.Server
	accessToken string
	// apiHost is the host the Responses API is served on; set it before the
	// first request.
	apiHost string

	mu        sync.Mutex
	exchanges []map[string]any
	requests  []*http.Request
	bodies    []map[string]any
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{accessToken: rand.Text(), apiHost: "api.openai.com"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding %s request: %v", r.URL.Path, err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch host := r.Header.Get("X-Fixture-Host"); {
		case host == "auth.openai.com" && r.URL.Path == "/oauth/token":
			f.exchanges = append(f.exchanges, body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.accessToken, "token_type": "Bearer", "expires_in": 3600})
		case host == f.apiHost && r.URL.Path == "/v1/responses":
			f.requests = append(f.requests, r)
			f.bodies = append(f.bodies, body)
			n := len(f.requests)
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
				"id": fmt.Sprintf("resp-%d", n), "status": "completed",
				"output": []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc-%d", n), "call_id": fmt.Sprintf("call-%d", n), "name": "submit_result", "arguments": `{"reasoning":"complete","result":{"answer":"done"}}`, "status": "completed"}},
				"usage":  map[string]any{"input_tokens": 7, "output_tokens": 3},
			}})
			_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", b)
		default:
			t.Errorf("unexpected request to %s%s", host, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// RoundTrip implements http.RoundTripper.
func (f *fakeOpenAI) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-HTTPS request to %s", r.URL)
	}
	target, err := url.Parse(f.server.URL)
	if err != nil {
		return nil, err
	}
	routed := r.Clone(r.Context())
	routed.Header.Set("X-Fixture-Host", r.URL.Host)
	routed.URL.Scheme, routed.URL.Host, routed.Host = target.Scheme, target.Host, ""
	return http.DefaultTransport.RoundTrip(routed)
}

// fakeGoogleIDTokenADC points GOOGLE_APPLICATION_CREDENTIALS at a throwaway
// service account key whose token endpoint is a local fake, so the adapter's
// real Application Default Credentials path mints the subject token. It
// returns that ID token and a count of Google token requests.
func fakeGoogleIDTokenADC(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"aud": openaiauth.Audience, "sub": rand.Text(), "exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	idToken := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims) + "." + enc([]byte(rand.Text()))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idToken})
	}))
	t.Cleanup(server.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": rand.Text(),
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		"client_email":   "fake@test-project.iam.gserviceaccount.com",
		"token_uri":      server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, creds, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	return idToken, &requests
}

// TestOpenAIDirectResponsesWorkloadIdentity runs two routed agents through
// one adapter against fakes of every remote: Google's token endpoint (through
// ADC), OpenAI's token exchange and the Responses API. The SDK's own workload
// identity code performs the exchange.
func TestOpenAIDirectResponsesWorkloadIdentity(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID"} {
		t.Setenv(name, rand.Text())
	}
	t.Setenv("OPENAI_BASE_URL", "https://unused.invalid/")
	idToken, googleRequests := fakeGoogleIDTokenADC(t)
	fake := newFakeOpenAI(t)
	cfg := openaiauth.Config{IdentityProviderID: "idp_" + rand.Text(), ServiceAccountID: rand.Text()}
	adapter, err := newOpenAIDirectResponsesAdapter(cfg, fake)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewOpenAIResponsesAdapterRegistry(OpenAIResponsesRegistration{Provider: modelrouter.ProviderOpenAI, Adapter: adapter})
	if err != nil {
		t.Fatal(err)
	}
	route := openAIDirectRoute()
	router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIResponses: registry})
	agentCfg := routedTestConfig(t)
	agentCfg.Effort = effort.XHigh
	for range 2 {
		agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, agentCfg)
		if err != nil {
			t.Fatal(err)
		}
		tracer := new(basetenRecordingTracer)
		result, err := agent.Execute(agenttrace.WithTracer[*basetenTestResponse](t.Context(), tracer), &testRequest{}, toolcall.EmptyTools{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer != "done" {
			t.Errorf("answer: got = %q, want = %q", result.Answer, "done")
		}
		if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 1 || tracer.traces[0].Turns[0].Provider != agenttrace.SystemOpenAI {
			t.Errorf("trace provider attribution missing or wrong: %+v", tracer.traces)
		}
	}

	wantExchange := map[string]any{
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token":        idToken,
		"subject_token_type":   "urn:ietf:params:oauth:token-type:id_token",
		"identity_provider_id": cfg.IdentityProviderID,
		"service_account_id":   cfg.ServiceAccountID,
	}
	// Both agents share the adapter's one exchange and cached bearer.
	if len(fake.exchanges) != 1 {
		t.Fatalf("token exchanges: got = %d, want = 1", len(fake.exchanges))
	}
	if diff := cmp.Diff(wantExchange, fake.exchanges[0]); diff != "" {
		t.Errorf("token exchange body (-want, +got):\n%s", diff)
	}
	if got := googleRequests.Load(); got != 1 {
		t.Errorf("Google ID-token requests: got = %d, want = 1", got)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("Responses requests: got = %d, want = 2", len(fake.requests))
	}
	for i, r := range fake.requests {
		if got, want := r.Header.Get("Authorization"), "Bearer "+fake.accessToken; got != want {
			t.Errorf("request %d Authorization: got = %q, want the exchanged bearer", i, got)
		}
		for _, header := range []string{"OpenAI-Organization", "OpenAI-Project"} {
			if r.Header.Get(header) != "" {
				t.Errorf("request %d: unexpected %s header from the environment", i, header)
			}
		}
		body := fake.bodies[i]
		if body["model"] != route.ProviderModelID || body["store"] != false || body["stream"] != true {
			t.Errorf("request %d controls: model=%v store=%v stream=%v", i, body["model"], body["store"], body["stream"])
		}
		if diff := cmp.Diff(map[string]any{"effort": "xhigh"}, body["reasoning"]); diff != "" {
			t.Errorf("request %d reasoning (-want, +got):\n%s", i, diff)
		}
	}
}

// TestOpenAIDirectResponsesBaseURL pins that Config.BaseURL replaces the
// default API host while the token exchange stays on auth.openai.com. A
// data-residency project's tokens are rejected by api.openai.com.
func TestOpenAIDirectResponsesBaseURL(t *testing.T) {
	fakeGoogleIDTokenADC(t)
	fake := newFakeOpenAI(t)
	fake.apiHost = "us.api.openai.com"
	cfg := openaiauth.Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa", BaseURL: "https://us.api.openai.com/v1"}
	adapter, err := newOpenAIDirectResponsesAdapter(cfg, fake)
	if err != nil {
		t.Fatal(err)
	}
	route := openAIDirectRoute()
	plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := adapter(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	service := binding.Responses()
	stream := service.NewStreaming(t.Context(), responses.ResponseNewParams{Model: route.ProviderModelID})
	for stream.Next() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.exchanges) != 1 || len(fake.requests) != 1 {
		t.Errorf("exchanges, Responses requests: got = %d, %d, want = 1, 1", len(fake.exchanges), len(fake.requests))
	}
}

// redirectingOpenAI answers Responses requests with a redirect to location
// and counts the requests that reach it; the token exchange goes to the
// wrapped fake.
type redirectingOpenAI struct {
	*fakeOpenAI
	status     int
	location   *url.URL
	redirected atomic.Int32
}

// RoundTrip implements http.RoundTripper.
func (r *redirectingOpenAI) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.URL.Host == "api.openai.com" && req.URL.Path == "/v1/responses":
		return &http.Response{
			StatusCode: r.status,
			Header:     http.Header{"Location": []string{r.location.String()}},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	case req.URL.Host == r.location.Host && req.URL.Path == r.location.Path:
		r.redirected.Add(1)
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
	default:
		return r.fakeOpenAI.RoundTrip(req)
	}
}

// TestOpenAIDirectResponsesRejectsRedirects pins the adapter's CheckRedirect.
// The SDK already refuses a redirect off the base URL's origin, so only the
// same-origin destination fails without the adapter's guard; the other host
// covers the SDK's check.
func TestOpenAIDirectResponsesRejectsRedirects(t *testing.T) {
	fakeGoogleIDTokenADC(t)
	for _, location := range []string{"https://api.openai.com/v1/redirected", "https://redirect.invalid/v1/responses"} {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(location+"/"+http.StatusText(status), func(t *testing.T) {
				testOpenAIDirectRedirect(t, status, location)
			})
		}
	}
}

func testOpenAIDirectRedirect(t *testing.T, status int, location string) {
	target, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	transport := &redirectingOpenAI{fakeOpenAI: newFakeOpenAI(t), status: status, location: target}
	adapter, err := newOpenAIDirectResponsesAdapter(openaiauth.Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	route := openAIDirectRoute()
	plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := adapter(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	service := binding.Responses()
	if _, err := service.New(t.Context(), responses.ResponseNewParams{Model: route.ProviderModelID}); err == nil {
		t.Error("redirect response: got success, want error")
	}
	if got := len(transport.exchanges); got != 1 {
		t.Errorf("token exchanges: got = %d, want = 1", got)
	}
	if got := transport.redirected.Load(); got != 0 {
		t.Errorf("redirected requests: got = %d, want = 0", got)
	}
}

func TestOpenAIDirectResponsesAdapterValidation(t *testing.T) {
	t.Parallel()
	valid := openaiauth.Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa"}
	for _, cfg := range []openaiauth.Config{{}, {IdentityProviderID: "idp_x"}, {ServiceAccountID: "sa"}} {
		if _, err := NewOpenAIDirectResponsesAdapter(cfg); !errors.Is(err, ErrInvalidAdapter) {
			t.Errorf("config %+v: got = %v, want ErrInvalidAdapter", cfg, err)
		}
	}
	if _, err := newOpenAIDirectResponsesAdapter(valid, nil); !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("nil transport: got = %v, want ErrInvalidAdapter", err)
	}
	adapter, err := NewOpenAIDirectResponsesAdapter(valid)
	if err != nil {
		t.Fatal(err)
	}
	// A route for another provider, or OpenAI attribution under another
	// provider, must not receive this account's credentials.
	foreign := responsesRoute()
	mislabeled := openAIDirectRoute()
	mislabeled.Attribution = modelrouter.Attribution{ProviderName: agenttrace.SystemBedrock, LegacySystem: agenttrace.SystemBedrock}
	for _, route := range []modelrouter.Route{foreign, mislabeled} {
		plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter(t.Context(), plan); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("route %v: got = %v, want ErrInvalidBinding", route.Selection, err)
		}
	}
}

func TestOpenAIBackendResolvesWithoutCredentials(t *testing.T) {
	t.Parallel()
	route := openAIDirectRoute()
	runtime, err := NewRuntime([]modelrouter.Route{route}, OpenAIBackend("", openaiauth.Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa"}))
	if err != nil {
		t.Fatal(err)
	}
	// The region is ignored, so both targets share one router.
	first, _, err := runtime.Target(TargetConfig{Provider: modelrouter.ProviderOpenAI, Model: route.Selection.LogicalModel, Region: "us"}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := runtime.Target(TargetConfig{Provider: modelrouter.ProviderOpenAI, Model: route.Selection.LogicalModel}).Resolve()
	if err != nil || first != second {
		t.Fatalf("region-independent router: err = %v, shared = %v", err, first == second)
	}
	if _, err := NewWithTarget[*testRequest](t.Context(), runtime.Target(TargetConfig{Provider: modelrouter.ProviderOpenAI, Model: route.Selection.LogicalModel}), routedTestConfig(t)); err != nil {
		t.Fatalf("NewWithTarget: %v", err)
	}

	bad, err := NewRuntime([]modelrouter.Route{route}, OpenAIBackend("", openaiauth.Config{IdentityProviderID: "idp_x"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bad.Target(TargetConfig{Provider: modelrouter.ProviderOpenAI, Model: route.Selection.LogicalModel}).Resolve(); !errors.Is(err, ErrInvalidAdapter) || !strings.Contains(err.Error(), "service account ID") {
		t.Errorf("missing service account: got = %v, want ErrInvalidAdapter naming it", err)
	}
}
