/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package systemone

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/google/go-cmp/cmp"
)

func hopperPlan(t *testing.T) modelrouter.Plan {
	t.Helper()
	registry, err := modelrouter.NewRegistry(modelrouter.Route{
		Selection:       modelrouter.Selection{Provider: modelrouter.ProviderHopper, LogicalModel: ModelHopper},
		Protocol:        modelrouter.ProtocolTypeSafeSystemOne,
		ProviderModelID: ModelHopper,
		Attribution:     modelrouter.Attribution{ProviderName: HopperProviderName, LegacySystem: HopperProviderName},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.Resolve(modelrouter.Selection{Provider: modelrouter.ProviderHopper, LogicalModel: ModelHopper})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestHopperRouteRoundTrip(t *testing.T) {
	t.Parallel()
	var ids []string
	var idsMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("unexpected Authorization: %q", auth)
		}
		var wire struct {
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if wire.Model != ModelHopper || len(wire.Questions) != 1 {
			t.Errorf("model = %q, questions = %d", wire.Model, len(wire.Questions))
		}
		for id := range wire.Questions {
			idsMu.Lock()
			ids = append(ids, id)
			idsMu.Unlock()
			var answer any
			switch id {
			case "a_noul":
				answer = map[string]any{"type": "noul", "noul": 0.6}
			case "b_choice":
				answer = map[string]any{"type": "choice", "choice": "yes", "probabilities": map[string]float64{"no": 0.2, "yes": 0.8}}
			case "c_score":
				answer = map[string]any{"type": "score", "probabilities": map[string]float64{"0": 0.1, "1": 0.3, "2": 0.6}}
			default:
				t.Errorf("unexpected question %q", id)
			}
			if err := json.NewEncoder(w).Encode(map[string]any{
				"model":   ModelHopper,
				"answers": map[string]any{id: answer},
				"usage":   map[string]int{"input_tokens": 10, "output_tokens": 0},
			}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}
	}))
	defer srv.Close()

	client, err := NewClient("", WithRoute(hopperPlan(t)), WithEndpoint(srv.URL+"/v1/systemone"), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Ask(t.Context(), Request{
		State: "A sample document.",
		Questions: map[string]Question{
			"a_noul":   Noul{Instructions: "Is it relevant?"},
			"b_choice": Choice{Instructions: "Decide.", Options: map[string]Content{"no": "No", "yes": "Yes"}},
			"c_score":  Score{Instructions: "Rate.", Levels: []Content{"low", "medium", "high"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	idsMu.Lock()
	gotIDs := slices.Clone(ids)
	idsMu.Unlock()
	if diff := cmp.Diff([]string{"a_noul", "b_choice", "c_score"}, gotIDs); diff != "" {
		t.Errorf("request order (-want +got):\n%s", diff)
	}
	want := &Response{
		Model: ModelHopper,
		Usage: Usage{InputTokens: 30},
		Answers: map[string]Answer{
			"a_noul":   NoulAnswer{Probability: 0.6},
			"b_choice": ChoiceAnswer{Choice: "yes", Probabilities: map[string]float64{"no": 0.2, "yes": 0.8}, Confidence: 0.8},
			"c_score": ScoreAnswer{Score: 1.5, Legend: map[string]string{"0": "low", "1": "medium", "2": "high"},
				Probabilities: map[string]float64{"0": 0.1, "1": 0.3, "2": 0.6}, Confidence: 0.6},
		},
	}
	if diff := cmp.Diff(want, resp); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
}

func TestHopperDirectClientAndValidation(t *testing.T) {
	t.Parallel()
	plan := hopperPlan(t)
	if _, err := NewClient("", WithHopper()); err == nil || !strings.Contains(err.Error(), "explicit endpoint") {
		t.Errorf("Hopper without endpoint: %v", err)
	}
	if _, err := NewClient("", WithRoute(plan)); err == nil || !strings.Contains(err.Error(), "explicit endpoint") {
		t.Errorf("Hopper route without endpoint: %v", err)
	}
	if _, err := NewClient("", WithEndpoint("http://localhost:8080/v1/systemone")); err == nil || !strings.Contains(err.Error(), "api key") {
		t.Errorf("TypeSafe without key: %v", err)
	}
	if _, err := NewClient("sk-typesafe", WithHopper(), WithEndpoint("http://localhost:8080/v1/systemone")); err == nil || !strings.Contains(err.Error(), "does not accept an API key") {
		t.Errorf("Hopper with key: %v", err)
	}
	if _, err := NewClient("sk-typesafe", WithRoute(plan), WithEndpoint("http://localhost:8080/v1/systemone")); err == nil || !strings.Contains(err.Error(), "does not accept an API key") {
		t.Errorf("Hopper route with key: %v", err)
	}
	for _, opts := range [][]Option{
		{WithHopper(), WithRoute(plan)},
		{WithRoute(plan), WithHopper()},
	} {
		if _, err := NewClient("", opts...); err == nil {
			t.Error("combining WithHopper and WithRoute succeeded")
		}
	}
	client, err := NewClient("", WithHopper(), WithEndpoint("http://localhost:8080/v1/systemone"))
	if err != nil {
		t.Fatal(err)
	}
	if client.defaultModel != ModelHopper || client.providerName != HopperProviderName {
		t.Errorf("direct Hopper defaults = %q, %q", client.defaultModel, client.providerName)
	}
}

func TestHopperStopsAfterFailedQuestion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		status      int
		secondModel string
	}{
		{name: "API failure", status: http.StatusServiceUnavailable},
		{name: "model mismatch", secondModel: "other-model"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var wire struct {
					Questions map[string]json.RawMessage `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(wire.Questions) != 1 {
					t.Errorf("questions: got = %d, want = 1", len(wire.Questions))
				}
				for id := range wire.Questions {
					if id == "c" {
						t.Error("sent question after a failure")
					}
					if id == "b" && tt.status != 0 {
						w.WriteHeader(tt.status)
						return
					}
					model := ModelHopper
					if id == "b" && tt.secondModel != "" {
						model = tt.secondModel
					}
					if err := json.NewEncoder(w).Encode(map[string]any{
						"model":   model,
						"answers": map[string]any{id: map[string]any{"type": "noul", "noul": 0.6}},
					}); err != nil {
						t.Errorf("encode response: %v", err)
					}
				}
			}))
			t.Cleanup(srv.Close)
			client, err := NewClient("", WithRoute(hopperPlan(t)), WithEndpoint(srv.URL+"/v1/systemone"),
				WithHTTPClient(srv.Client()), WithRetryConfig(fastRetry(0)))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Ask(t.Context(), Request{State: "document", Questions: map[string]Question{
				"a": Noul{Instructions: "First?"},
				"b": Noul{Instructions: "Second?"},
				"c": Noul{Instructions: "Third?"},
			}})
			if resp != nil || err == nil || !strings.Contains(err.Error(), `question "b"`) {
				t.Fatalf("Ask: got = (%v, %v), want nil and error for question b", resp, err)
			}
			if got := calls.Load(); got != 2 {
				t.Errorf("HTTP calls: got = %d, want = 2", got)
			}
			if tt.status != 0 {
				apiErr, ok := errors.AsType[*APIError](err)
				if !ok || apiErr.StatusCode != tt.status || !IsRetryable(err) {
					t.Errorf("API error: got = %v, want retryable status %d", err, tt.status)
				}
			} else if !errors.Is(err, ErrResponseValidation) {
				t.Errorf("model mismatch error: got = %v, want ErrResponseValidation", err)
			}
		})
	}
}

func TestHopperNormalizesRoundedScore(t *testing.T) {
	t.Parallel()
	questions := map[string]Question{"q": Score{Instructions: "Rate.", Levels: []Content{"0", "1", "2", "3", "4"}}}
	body := []byte(`{"model":"hopper","answers":{"q":{"type":"score","probabilities":{"0":0,"1":0,"2":0,"3":0.005,"4":1.0}}}}`)
	resp, err := decodeResponse(body, questions, true)
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Answers["q"].(ScoreAnswer).Score
	want := 4.015 / 1.005
	if got != want {
		t.Errorf("score: got = %v, want = %v", got, want)
	}
}

func TestHopperStillValidatesDistribution(t *testing.T) {
	t.Parallel()
	questions := map[string]Question{"q": Choice{Instructions: "Decide.", Options: map[string]Content{"no": "No", "yes": "Yes"}}}
	body := []byte(`{"model":"hopper","answers":{"q":{"type":"choice","choice":"yes","probabilities":{"yes":0.8}}}}`)
	if _, err := decodeResponse(body, questions, true); err == nil {
		t.Error("incomplete Hopper probabilities were accepted")
	}
	if _, err := decodeResponse(body, questions, false); err == nil {
		t.Error("incomplete TypeSafe probabilities were accepted")
	}
}
