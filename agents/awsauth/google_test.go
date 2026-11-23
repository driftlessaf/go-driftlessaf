/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package awsauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	httpmetrics "github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
)

func TestGoogleConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		env    map[string]string
		want   string
	}{
		{name: "missing role", config: Config{Google: GoogleConfig{Audience: "aud"}}, want: "role ARN and audience"},
		{name: "missing audience", config: Config{Google: GoogleConfig{RoleARN: "role"}}, want: "role ARN and audience"},
		{name: "typed profile", config: Config{Profile: "sso", Google: GoogleConfig{RoleARN: "role", Audience: "aud"}}, want: "cannot be combined"},
		{name: "ambient profile", env: map[string]string{EnvProfile: "sso"}, want: "cannot be combined"},
		{name: "ambient file", env: map[string]string{EnvWebIdentityTokenFile: "token"}, want: "cannot be combined"},
		{name: "different role", env: map[string]string{EnvRoleARN: "other"}, want: "role conflicts"},
		{name: "different audience", env: map[string]string{EnvGoogleAudience: "other"}, want: "audience conflicts"},
		{name: "static key", env: map[string]string{envAccessKeyID: "secret"}, want: "static credentials"},
		{name: "API key", env: map[string]string{envBearerToken: "secret"}, want: "API-key"},
		{name: "user email", config: Config{Google: GoogleConfig{RoleARN: "role", Audience: "aud", ServiceAccount: "someone@example.com"}}, want: "service account must be"},
		{name: "path in account", config: Config{Google: GoogleConfig{RoleARN: "role", Audience: "aud", ServiceAccount: "a/b@p.iam.gserviceaccount.com"}}, want: "service account must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnvironment(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg := tt.config
			if cfg == (Config{}) {
				cfg.Google = GoogleConfig{RoleARN: "role", Audience: "aud"}
			}
			_, err := cfg.LoadAWSConfig(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadAWSConfig() error = %v, want %q", err, tt.want)
			}
		})
	}
}

const googleTestRole = "arn:aws:iam::123456789012:role/BedrockInvoker"

func TestGoogleConfigFromEnv(t *testing.T) {
	clearEnvironment(t)
	t.Setenv(EnvRegion, "us-east-1")
	t.Setenv(EnvRoleARN, googleTestRole)
	t.Setenv(EnvGoogleAudience, "https://aws.example/skillup")
	cfg, err := ConfigFromEnv(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Google.RoleARN != googleTestRole || cfg.Google.Audience != "https://aws.example/skillup" {
		t.Fatalf("unexpected Google configuration: %#v", cfg.Google)
	}
	if err := (Config{Region: "us-east-1"}).ValidateCredentials(t.Context()); err == nil {
		t.Fatal("ambient audience must not implicitly select Google")
	}
}

func TestGoogleCredentialRefresh(t *testing.T) {
	clearEnvironment(t)
	var metadataCalls, stsCalls atomic.Int32
	var rejectMetadata, rejectSTS atomic.Bool
	audience := "https://aws.example/workload?name=a&b=c"
	metadataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := metadataCalls.Add(1)
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" || r.URL.Query().Get("audience") != audience || r.URL.Query().Get("format") != "full" || r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("unexpected metadata request: %s", r.URL)
		}
		if rejectMetadata.Load() {
			http.Error(w, "sensitive-token", http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, "identity-%d", n)
	}))
	t.Cleanup(metadataServer.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadataServer.URL, "http://"))
	stsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := stsCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		for k, want := range map[string]string{"Action": "AssumeRoleWithWebIdentity", "RoleArn": googleTestRole, "WebIdentityToken": fmt.Sprintf("identity-%d", metadataCalls.Load()), "DurationSeconds": "3600"} {
			if r.Form.Get(k) != want {
				t.Errorf("STS field %s mismatch", k)
			}
		}
		w.Header().Set("Content-Type", "application/xml")
		if rejectSTS.Load() {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<ErrorResponse><Error><Code>InvalidIdentityToken</Code><Message>sensitive-token</Message></Error></ErrorResponse>`)
			return
		}
		expiration := time.Now().Add(time.Hour)
		if n == 1 {
			expiration = time.Now().Add(30 * time.Second)
		}
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>key-%d</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>session</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, n, expiration.UTC().Format(time.RFC3339))
	}))
	t.Cleanup(stsServer.Close)
	t.Setenv("AWS_ENDPOINT_URL_STS", stsServer.URL)
	ctx, cancel := context.WithCancel(t.Context())
	cfg, err := (Config{Region: "us-east-1", Google: GoogleConfig{RoleARN: googleTestRole, Audience: audience}}).LoadAWSConfig(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := cfg.Credentials.Retrieve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessKeyID != "key-2" || metadataCalls.Load() != 2 || stsCalls.Load() != 2 {
		t.Fatal("expired credentials did not refresh with a fresh Google token after startup context ended")
	}
	if _, err := cfg.Credentials.Retrieve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stsCalls.Load() != 2 {
		t.Fatal("valid credentials were not cached")
	}
	cache, ok := cfg.Credentials.(*aws.CredentialsCache)
	if !ok {
		t.Fatalf("credential provider: got = %T, want *aws.CredentialsCache", cfg.Credentials)
	}
	for _, failure := range []struct {
		name string
		flag *atomic.Bool
	}{{"metadata", &rejectMetadata}, {"STS", &rejectSTS}} {
		t.Run(failure.name, func(t *testing.T) {
			cache.Invalidate()
			failure.flag.Store(true)
			_, err := cache.Retrieve(t.Context())
			if err == nil || strings.Contains(err.Error(), "sensitive-token") {
				t.Fatalf("credential error: got = %v, want sanitized failure", err)
			}
			failure.flag.Store(false)
			if _, err := cache.Retrieve(t.Context()); err != nil {
				t.Fatalf("failure was latched: %v", err)
			}
		})
	}
}

func TestGoogleRejectsInvalidMetadata(t *testing.T) {
	for _, kind := range []string{"empty", "oversized", "redirect", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			clearEnvironment(t)
			var metadataCalls atomic.Int32
			var downstreamCalls atomic.Int32
			downstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { downstreamCalls.Add(1) }))
			t.Cleanup(downstream.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadataCalls.Add(1)
				switch kind {
				case "oversized":
					fmt.Fprint(w, strings.Repeat("x", (64<<10)+1))
				case "redirect":
					http.Redirect(w, r, downstream.URL, http.StatusFound)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(server.URL, "http://"))
			t.Setenv("AWS_ENDPOINT_URL_STS", downstream.URL)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			_, err := (Config{Region: "us-east-1", Google: GoogleConfig{RoleARN: googleTestRole, Audience: "aud"}}).LoadAWSConfig(ctx)
			if err == nil {
				t.Fatal("invalid metadata accepted")
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if kind == "cancelled" && metadataCalls.Load() != 0 {
				t.Fatal("cancelled construction requested a metadata token")
			}
			if downstreamCalls.Load() != 0 {
				t.Fatal("invalid metadata reached redirect target or STS")
			}
		})
	}
}

// writeADC points Application Default Credentials at a fake authorized_user
// file whose refresh token is exchanged at tokenURL.
func writeADC(t *testing.T, tokenURL string) {
	t.Helper()
	credentials, err := json.Marshal(map[string]string{
		"type": "authorized_user", "client_id": "client", "client_secret": "secret",
		"refresh_token": "refresh", "token_uri": tokenURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(path, credentials, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

const metadataTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"

func TestGoogleServiceAccountIDToken(t *testing.T) {
	const account = "code-reviewer-bedrock@example-project.iam.gserviceaccount.com"
	audience := "https://aws.example/workload?name=a&b=c"
	tests := []struct {
		name           string
		serviceAccount string
		// adc writes an ADC file that must never authorize the call.
		adc          bool
		wantToken    string
		wantIdentity int32
		wantIAM      int32
	}{
		{name: "metadata server", wantToken: "metadata-identity", wantIdentity: 2},
		{name: "service account", serviceAccount: account, wantToken: "service-account-identity", wantIAM: 2},
		{name: "service account ignores ADC", serviceAccount: account, adc: true, wantToken: "service-account-identity", wantIAM: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnvironment(t)
			// Services such as github-bots wrap the default transport with metrics.
			previousTransport := http.DefaultTransport
			http.DefaultTransport = httpmetrics.WrapTransport(previousTransport)
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			// The SDK's debug logs go to stderr and must never carry a token.
			t.Setenv("GOOGLE_SDK_GO_LOGGING_LEVEL", "debug")
			stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			previousStderr := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() {
				os.Stderr = previousStderr
				// An open file keeps t.TempDir from removing it on Windows.
				if err := stderr.Close(); err != nil {
					t.Errorf("closing stderr capture: %v", err)
				}
			})

			var identityCalls, tokenCalls, iamCalls, adcCalls atomic.Int32
			var rejectIAM atomic.Bool
			metadataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Metadata-Flavor") != "Google" {
					t.Errorf("metadata request without Metadata-Flavor: %s", r.URL)
				}
				if r.URL.Path == metadataTokenPath {
					n := tokenCalls.Add(1)
					if r.URL.Query().Get("scopes") != iamcredentials.CloudPlatformScope {
						t.Errorf("unexpected metadata token scopes: %s", r.URL)
					}
					// Expires inside the refresh window, so every IAM call needs a new token.
					fmt.Fprintf(w, `{"access_token":"metadata-access-%d","token_type":"Bearer","expires_in":30}`, n)
					return
				}
				identityCalls.Add(1)
				if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" || r.URL.Query().Get("audience") != audience || r.URL.Query().Get("format") != "full" {
					t.Errorf("unexpected metadata request: %s", r.URL)
				}
				fmt.Fprint(w, "metadata-identity")
			}))
			t.Cleanup(metadataServer.Close)
			t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadataServer.URL, "http://"))
			if tt.adc {
				adcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					adcCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"access_token":"adc-access","token_type":"Bearer","expires_in":3600}`)
				}))
				t.Cleanup(adcServer.Close)
				writeADC(t, adcServer.URL)
			}
			// Mirrors iamcredentials.projects.serviceAccounts.generateIdToken.
			iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				iamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/-/serviceAccounts/"+account+":generateIdToken" {
					t.Errorf("unexpected IAM Credentials request: %s %s", r.Method, r.URL.Path)
				}
				if got, want := r.Header.Get("Authorization"), fmt.Sprintf("Bearer metadata-access-%d", tokenCalls.Load()); got != want {
					t.Errorf("Authorization: got = %q, want = %q", got, want)
				}
				var got iamcredentials.GenerateIdTokenRequest
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if diff := cmp.Diff(iamcredentials.GenerateIdTokenRequest{Audience: audience, IncludeEmail: true}, got); diff != "" {
					t.Errorf("generateIdToken body (-want +got):\n%s", diff)
				}
				if rejectIAM.Load() {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":{"code":403,"message":"sensitive-token","status":"PERMISSION_DENIED"}}`)
					return
				}
				fmt.Fprint(w, `{"token":"service-account-identity"}`)
			}))
			t.Cleanup(iamServer.Close)
			previous := iamCredentialsOptions
			iamCredentialsOptions = []option.ClientOption{option.WithEndpoint(iamServer.URL + "/")}
			t.Cleanup(func() { iamCredentialsOptions = previous })
			var stsCalls atomic.Int32
			stsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stsCalls.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if got := r.Form.Get("WebIdentityToken"); got != tt.wantToken {
					t.Errorf("WebIdentityToken: got = %q, want = %q", got, tt.wantToken)
				}
				w.Header().Set("Content-Type", "application/xml")
				fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>key</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>session</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			}))
			t.Cleanup(stsServer.Close)
			t.Setenv("AWS_ENDPOINT_URL_STS", stsServer.URL)

			ctx, cancel := context.WithCancel(t.Context())
			cfg, err := (Config{Region: "us-east-1", Google: GoogleConfig{RoleARN: googleTestRole, Audience: audience, ServiceAccount: tt.serviceAccount}}).LoadAWSConfig(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			cache, ok := cfg.Credentials.(*aws.CredentialsCache)
			if !ok {
				t.Fatalf("credential provider: got = %T, want *aws.CredentialsCache", cfg.Credentials)
			}
			// Refresh after the construction context ends; the access token must refresh too.
			cache.Invalidate()
			if _, err := cache.Retrieve(t.Context()); err != nil {
				t.Fatal(err)
			}
			if identityCalls.Load() != tt.wantIdentity || iamCalls.Load() != tt.wantIAM || tokenCalls.Load() != tt.wantIAM || stsCalls.Load() != 2 {
				t.Fatalf("calls: identity = %d, IAM = %d, access token = %d, STS = %d; want %d, %d, %d, 2",
					identityCalls.Load(), iamCalls.Load(), tokenCalls.Load(), stsCalls.Load(), tt.wantIdentity, tt.wantIAM, tt.wantIAM)
			}
			if adcCalls.Load() != 0 {
				t.Fatalf("ADC token endpoint called %d times; only the metadata server may authorize", adcCalls.Load())
			}
			if tt.serviceAccount != "" {
				cache.Invalidate()
				rejectIAM.Store(true)
				_, err = cache.Retrieve(t.Context())
				if err == nil || strings.Contains(err.Error(), "sensitive-token") || !strings.Contains(err.Error(), "HTTP 403") {
					t.Fatalf("credential error: got = %v, want sanitized HTTP 403", err)
				}
				if stsCalls.Load() != 2 {
					t.Fatal("failed ID token reached STS")
				}
			}
			logs, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{tt.wantToken, "metadata-access", "adc-access", "sensitive-token"} {
				if strings.Contains(string(logs), secret) {
					t.Errorf("stderr contains %q:\n%s", secret, logs)
				}
			}
		})
	}
}

func TestGoogleServiceAccountAccessTokenIsBounded(t *testing.T) {
	clearEnvironment(t)
	release := make(chan struct{})
	var iamCalls atomic.Int32
	metadataServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(metadataServer.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadataServer.URL, "http://"))
	iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		iamCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(iamServer.Close)
	previous := iamCredentialsOptions
	iamCredentialsOptions = []option.ClientOption{option.WithEndpoint(iamServer.URL + "/")}
	t.Cleanup(func() { iamCredentialsOptions = previous })
	cfg, err := (Config{Region: "us-east-1", Google: GoogleConfig{
		RoleARN: googleTestRole, Audience: "aud", ServiceAccount: "code-reviewer-bedrock@example-project.iam.gserviceaccount.com",
	}}).loadGoogleConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := cfg.Credentials.Retrieve(t.Context())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled access token request produced credentials")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("stalled metadata access token request was not bounded")
	}
	if iamCalls.Load() != 0 {
		t.Fatal("IAM Credentials was called without an access token")
	}
}
