/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"golang.org/x/oauth2"
)

func TestVertexResponsesTransport(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID"} {
		t.Setenv(name, rand.Text())
	}
	t.Setenv("OPENAI_BASE_URL", "https://unused.invalid/")
	for _, tc := range []struct{ region, host string }{
		{region: "global", host: "aiplatform.googleapis.com"},
		{region: "us", host: "aiplatform.us.rep.googleapis.com"},
		{region: "eu", host: "aiplatform.eu.rep.googleapis.com"},
		{region: "us-central1", host: "us-central1-aiplatform.googleapis.com"},
	} {
		t.Run(tc.region, func(t *testing.T) {
			token := rand.Text()
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
					t.Error("OAuth authorization missing")
				}
				for _, name := range []string{"OpenAI-Organization", "OpenAI-Project"} {
					if r.Header.Get(name) != "" {
						t.Errorf("unexpected header %q", name)
					}
				}
				if got, want := r.URL.Path, "/v1/projects/test-project/locations/"+tc.region+"/endpoints/openapi/responses"; got != want {
					t.Errorf("path: got = %q, want = %q", got, want)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["store"] != false {
					t.Error("store must be false")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = fmt.Fprint(w, `{"error":{"message":"fixture"}}`)
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := newVertexOpenAIResponsesAdapter("test-project", tc.region, func(_ context.Context, scopes ...string) (oauth2.TokenSource, error) {
				if len(scopes) != 1 || scopes[0] != vertexCloudPlatformScope {
					t.Errorf("unexpected scopes: %v", scopes)
				}
				return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			route := vertexRouterTestRoute(modelrouter.ProtocolOpenAIResponses, "example/responses-model")
			plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := adapter(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			service := binding.Responses()
			_, err = service.New(t.Context(), responses.ResponseNewParams{Model: route.ProviderModelID}, option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
				if r.URL.Host != tc.host || r.URL.Scheme != "https" {
					t.Errorf("endpoint: got = %s, want HTTPS host %q", r.URL, tc.host)
				}
				r.URL.Scheme = target.Scheme
				r.URL.Host = target.Host
				return next(r)
			}))
			if err == nil || requests != 1 {
				t.Errorf("request count: got = %d, want = 1 with error", requests)
			}
			if got := binding.ResourceLabels(); got["projectID"] != "test-project" || got["region"] != tc.region || got["model_name"] != route.ProviderModelID {
				t.Errorf("labels: got = %v", got)
			}
		})
	}
}

func TestVertexResponsesRejectsRedirects(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var redirected atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirected.Add(1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer destination.Close()
			token := rand.Text()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("OAuth authorization missing")
				}
				// A different hostname exercises cross-host credential handling.
				w.Header().Set("Location", strings.Replace(destination.URL, "127.0.0.1", "localhost", 1))
				w.WriteHeader(status)
			}))
			defer server.Close()
			adapter, err := newVertexOpenAIResponsesAdapter("test-project", "global", func(context.Context, ...string) (oauth2.TokenSource, error) {
				return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			route := vertexRouterTestRoute(modelrouter.ProtocolOpenAIResponses, "example/responses-model")
			plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := adapter(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			service := binding.Responses()
			_, err = service.New(t.Context(), responses.ResponseNewParams{Model: route.ProviderModelID}, option.WithBaseURL(server.URL))
			if err == nil {
				t.Error("redirect response: got success, want error")
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("endpoint requests: got = %d, want = 1", got)
			}
			if got := redirected.Load(); got != 0 {
				t.Errorf("redirected requests: got = %d, want = 0", got)
			}
		})
	}
}

func TestVertexResponsesLazyValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ project, region string }{{"", "global"}, {"test/project", "global"}, {"test-project", ""}} {
		if _, err := NewVertexOpenAIResponsesAdapter(tc.project, tc.region); !errors.Is(err, ErrInvalidAdapter) {
			t.Errorf("invalid config: got = %v", err)
		}
	}
	if _, err := newVertexOpenAIResponsesAdapter("test-project", "global", nil); !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("nil factory: got = %v", err)
	}
	calls := 0
	want := errors.New("credential failure")
	adapter, err := newVertexOpenAIResponsesAdapter("test-project", "global", func(context.Context, ...string) (oauth2.TokenSource, error) { calls++; return nil, want })
	if err != nil || calls != 0 {
		t.Fatalf("construction: err = %v, credential calls = %d", err, calls)
	}
	foreign := responsesRoute()
	foreignPlan, err := mustRouteRegistry(t, foreign).Resolve(foreign.Selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter(t.Context(), foreignPlan); !errors.Is(err, ErrInvalidBinding) || calls != 0 {
		t.Fatalf("foreign route: err = %v, credential calls = %d", err, calls)
	}
	route := vertexRouterTestRoute(modelrouter.ProtocolOpenAIResponses, "example/responses-model")
	plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter(t.Context(), plan); !errors.Is(err, want) || calls != 1 {
		t.Fatalf("binding: err = %v, credential calls = %d", err, calls)
	}
}
