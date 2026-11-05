/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/auth"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr []string
	}{
		{name: "both set", cfg: Config{IdentityProviderID: "idp_" + rand.Text(), ServiceAccountID: rand.Text()}},
		{name: "neither set", cfg: Config{}, wantErr: []string{"identity provider ID", "service account ID"}},
		{name: "missing service account", cfg: Config{IdentityProviderID: "idp_" + rand.Text()}, wantErr: []string{"requires a service account ID"}},
		{name: "missing identity provider", cfg: Config{ServiceAccountID: rand.Text()}, wantErr: []string{"requires a identity provider ID"}},
		{name: "padded", cfg: Config{IdentityProviderID: " idp_x", ServiceAccountID: "sa\n"}, wantErr: []string{"identity provider ID must not", "service account ID must not"}},
		{name: "regional base URL", cfg: Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa", BaseURL: "https://us.api.openai.com/v1"}},
		{name: "http base URL", cfg: Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa", BaseURL: "http://us.api.openai.com/v1"}, wantErr: []string{"base URL"}},
		{name: "relative base URL", cfg: Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa", BaseURL: "/v1"}, wantErr: []string{"base URL"}},
		{name: "base URL with credentials", cfg: Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa", BaseURL: "https://u:p@us.api.openai.com/v1"}, wantErr: []string{"base URL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate: got = %v, want = nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate: got = nil, want an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate: got = %v, want containing %q", err, want)
				}
			}
			if _, err := tc.cfg.WorkloadIdentity(); err == nil {
				t.Error("WorkloadIdentity: got = nil, want the validation error")
			}
		})
	}
}

func TestWorkloadIdentityCarriesIDs(t *testing.T) {
	t.Parallel()
	cfg := Config{IdentityProviderID: "idp_" + rand.Text(), ServiceAccountID: rand.Text()}
	wi, err := cfg.WorkloadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if wi.IdentityProviderID != cfg.IdentityProviderID || wi.ServiceAccountID != cfg.ServiceAccountID {
		t.Errorf("WorkloadIdentity IDs: got = %q, %q, want = %q, %q", wi.IdentityProviderID, wi.ServiceAccountID, cfg.IdentityProviderID, cfg.ServiceAccountID)
	}
	if got := wi.Provider.TokenType(); got != auth.SubjectTokenTypeID {
		t.Errorf("TokenType: got = %q, want = %q", got, auth.SubjectTokenTypeID)
	}
	// The SDK validates the configuration on first use; fail here instead.
	if _, err := auth.NewWorkloadIdentityAuth(wi); err != nil {
		t.Errorf("NewWorkloadIdentityAuth: %v", err)
	}
}

// TestGoogleIDTokenFromADC drives the real idtoken path through Application
// Default Credentials: a service account key whose token_uri is a fake Google
// token endpoint. It proves the requested audience and that the ID token is
// fetched once and then reused from the source's cache.
func TestGoogleIDTokenFromADC(t *testing.T) {
	forEachAuthLib(t, func(t *testing.T) {
		idToken, requests := fakeGoogleADC(t, nil)
		wi, err := Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa"}.WorkloadIdentity()
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			got, err := wi.Provider.GetToken(t.Context(), nil)
			if err != nil {
				t.Fatalf("GetToken: %v", err)
			}
			if got != idToken {
				t.Errorf("GetToken: got = %q, want = the fake ID token", got)
			}
		}
		if got := requests.Load(); got != 1 {
			t.Errorf("Google token requests: got = %d, want = 1", got)
		}
	})
}

// TestGoogleIDTokenHonorsContext holds the Google token endpoint open, cancels
// the caller once the fetch has started, and checks that GetToken returns
// before the endpoint answers. The abandoned fetch then completes and fills
// the cache, so the next call makes no new request.
func TestGoogleIDTokenHonorsContext(t *testing.T) {
	forEachAuthLib(t, func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		idToken, requests := fakeGoogleADC(t, func() {
			once.Do(func() { close(started) })
			<-release
		})
		// Registered after the fake server, so it runs first and a failed test
		// does not leave server.Close waiting on the held handler.
		unblock := sync.OnceFunc(func() { close(release) })
		t.Cleanup(unblock)
		wi, err := Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa"}.WorkloadIdentity()
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() {
			_, err := wi.Provider.GetToken(ctx, nil)
			errc <- err
		}()
		<-started
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("GetToken after cancel: got = %v, want = context.Canceled", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("GetToken did not return after its context was cancelled")
		}

		unblock()
		got, err := wi.Provider.GetToken(t.Context(), nil)
		if err != nil {
			t.Fatalf("GetToken after release: %v", err)
		}
		if got != idToken {
			t.Errorf("GetToken after release: got = %q, want = the fake ID token", got)
		}
		if got := requests.Load(); got != 1 {
			t.Errorf("Google token requests: got = %d, want = 1", got)
		}
	})
}

// forEachAuthLib runs test once against each idtoken implementation: the
// oauth2 flow and the newer cloud.google.com/go/auth library.
func forEachAuthLib(t *testing.T, test func(t *testing.T)) {
	t.Helper()
	for _, tc := range []struct {
		name    string
		newAuth bool
	}{{name: "oauth2"}, {name: "new auth lib", newAuth: true}} {
		t.Run(tc.name, func(t *testing.T) {
			useAuthLib(t, tc.newAuth)
			test(t)
		})
	}
}

// useAuthLib picks the idtoken implementation for the rest of the test. Both
// variables are set, since the disable switch wins over the enable switch.
func useAuthLib(t *testing.T, newAuth bool) {
	t.Helper()
	t.Setenv("GOOGLE_API_GO_EXPERIMENTAL_DISABLE_NEW_AUTH_LIB", strconv.FormatBool(!newAuth))
	t.Setenv("GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB", strconv.FormatBool(newAuth))
}

// TestGoogleIDTokenStalledEndpointRecovers holds the Google token endpoint
// open past the provider's timeout. Concurrent callers share one fetch, which
// fails at the timeout, and once the endpoint answers again the next call
// starts a fresh fetch and succeeds. It pins the oauth2 flow: the newer auth
// library's service-account path ignores the provider's timeout and keeps its
// own 30-second one.
func TestGoogleIDTokenStalledEndpointRecovers(t *testing.T) {
	useAuthLib(t, false)
	release := make(chan struct{})
	idToken, requests := fakeGoogleADC(t, func() { <-release })
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	wi, err := Config{IdentityProviderID: "idp_x", ServiceAccountID: "sa"}.WorkloadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	wi.Provider.(*googleIDTokenProvider).timeout = 200 * time.Millisecond

	const callers = 3
	errc := make(chan error, callers)
	for range callers {
		go func() {
			_, err := wi.Provider.GetToken(t.Context(), nil)
			errc <- err
		}()
	}
	for range callers {
		select {
		case err := <-errc:
			if err == nil {
				t.Error("GetToken against a stalled endpoint: got = nil, want an error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("GetToken did not fail after the fetch timeout")
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("Google token requests while stalled: got = %d, want = 1", got)
	}

	unblock()
	got, err := wi.Provider.GetToken(t.Context(), nil)
	if err != nil {
		t.Fatalf("GetToken after recovery: %v", err)
	}
	if got != idToken {
		t.Errorf("GetToken after recovery: got = %q, want = the fake ID token", got)
	}
}

// fakeGoogleADC points GOOGLE_APPLICATION_CREDENTIALS at a throwaway service
// account key whose token endpoint is a local fake. The fake runs hold, when
// set, before answering; checks that the signed assertion asks for an ID token
// for Audience; and answers with an unsigned JWT, which idtoken parses only
// for its expiry. It returns that JWT and a count of token requests.
func fakeGoogleADC(t *testing.T, hold func()) (string, *atomic.Int32) {
	t.Helper()
	idToken := unsignedJWT(t, map[string]any{"aud": Audience, "sub": rand.Text(), "exp": time.Now().Add(time.Hour).Unix()})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if hold != nil {
			hold()
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing token request: %v", err)
		}
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		if len(parts) != 3 {
			t.Errorf("assertion: got %d segments, want 3", len(parts))
			http.Error(w, "bad assertion", http.StatusBadRequest)
			return
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Errorf("decoding assertion: %v", err)
		}
		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Errorf("parsing assertion claims: %v", err)
		}
		if got := claims["target_audience"]; got != Audience {
			t.Errorf("target_audience: got = %v, want = %q", got, Audience)
		}
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

func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(payload) + "." + enc([]byte(rand.Text()))
}
