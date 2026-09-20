/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package awsauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
