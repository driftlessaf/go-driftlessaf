/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudebackend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestResolveWithConfig(t *testing.T) {
	setFakeADC(t)

	tests := []struct {
		name         string
		model        string
		config       config
		wantModel    string
		wantProvider claudeexecutor.Provider
	}{
		{
			name:         "Vertex preserves model ID",
			model:        "claude-sonnet-4-6@default",
			config:       config{backend: backendVertex},
			wantModel:    "claude-sonnet-4-6@default",
			wantProvider: claudeexecutor.ProviderVertex,
		},
		{
			name:  "Anthropic direct removes Vertex version",
			model: "claude-sonnet-4-6@default",
			config: config{
				backend: backendAnthropic,
				anthropic: anthropicauth.Config{
					FederationRuleID: "fdrl_0123456789",
					OrganizationID:   "12345678-1234-1234-1234-123456789012",
				},
			},
			wantModel:    "claude-sonnet-4-6",
			wantProvider: claudeexecutor.ProviderAnthropic,
		},
		{
			name:  "Anthropic direct preserves first-party model ID",
			model: "claude-haiku-4-5",
			config: config{
				backend: backendAnthropic,
				anthropic: anthropicauth.Config{
					FederationRuleID: "fdrl_9876543210",
					OrganizationID:   "87654321-4321-4321-4321-210987654321",
				},
			},
			wantModel:    "claude-haiku-4-5",
			wantProvider: claudeexecutor.ProviderAnthropic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolve(t.Context(), "my-project", "us-central1", tt.model, tt.config, func(context.Context, awsauth.Config) (anthropic.MessageService, error) {
				t.Fatal("Mantle factory called for non-Bedrock backend")
				return anthropic.MessageService{}, nil
			})
			if err != nil {
				t.Fatalf("resolve(): got error %v, want nil", err)
			}
			if got.ModelID != tt.wantModel {
				t.Errorf("ModelID: got = %q, want = %q", got.ModelID, tt.wantModel)
			}
			if got.Provider != tt.wantProvider {
				t.Errorf("Provider: got = %q, want = %q", got.Provider, tt.wantProvider)
			}
			if len(got.Messages.Options) == 0 {
				t.Error("Messages.Options: got = empty, want configured service")
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config
		wantErr string
	}{
		{
			name: "unset defaults to Vertex",
			want: config{backend: backendVertex},
		},
		{
			name: "explicit Vertex",
			env:  map[string]string{envBackend: string(backendVertex)},
			want: config{backend: backendVertex},
		},
		{
			name:    "explicit Anthropic requires profile",
			env:     map[string]string{envBackend: string(backendAnthropic)},
			wantErr: anthropicauth.EnvProfile,
		},
		{
			name: "Vertex conflicts with Anthropic profile",
			env: map[string]string{
				envBackend:               string(backendVertex),
				anthropicauth.EnvProfile: "configured",
			},
			wantErr: "conflicts",
		},
		{
			name: "Bedrock accepts SSO profile",
			env: map[string]string{
				envBackend:         string(backendBedrock),
				awsauth.EnvRegion:  "us-east-1",
				awsauth.EnvProfile: "dev-sso",
			},
			want: config{
				backend: backendBedrock,
				aws:     awsauth.Config{Region: "us-east-1", Profile: "dev-sso"},
			},
		},
		{
			name: "Bedrock accepts web identity",
			env: map[string]string{
				envBackend:                      string(backendBedrock),
				awsauth.EnvRegion:               "us-east-2",
				awsauth.EnvRoleARN:              "arn:aws:iam::123456789012:role/presubmit",
				awsauth.EnvWebIdentityTokenFile: "/var/run/secrets/aws/token",
			},
			want: config{backend: backendBedrock, aws: awsauth.Config{Region: "us-east-2"}},
		},
		{
			name: "Bedrock conflicts with Anthropic profile",
			env: map[string]string{
				envBackend:               string(backendBedrock),
				anthropicauth.EnvProfile: "configured",
			},
			wantErr: "conflicts",
		},
		{
			name: "Bedrock requires region",
			env: map[string]string{
				envBackend:         string(backendBedrock),
				awsauth.EnvProfile: "dev-sso",
			},
			wantErr: awsauth.EnvRegion,
		},
		{
			name: "Bedrock requires supported auth mode",
			env: map[string]string{
				envBackend:        string(backendBedrock),
				awsauth.EnvRegion: "us-east-1",
			},
			wantErr: awsauth.EnvProfile,
		},
		{
			name: "Bedrock rejects incomplete web identity",
			env: map[string]string{
				envBackend:         string(backendBedrock),
				awsauth.EnvRegion:  "us-east-1",
				awsauth.EnvRoleARN: "arn:aws:iam::123456789012:role/presubmit",
			},
			wantErr: "must be set together",
		},
		{
			name: "Bedrock rejects ambiguous auth modes",
			env: map[string]string{
				envBackend:                      string(backendBedrock),
				awsauth.EnvRegion:               "us-east-1",
				awsauth.EnvProfile:              "dev-sso",
				awsauth.EnvRoleARN:              "arn:aws:iam::123456789012:role/presubmit",
				awsauth.EnvWebIdentityTokenFile: "/var/run/secrets/aws/token",
			},
			wantErr: "cannot be combined",
		},
		{
			name:    "unknown backend",
			env:     map[string]string{envBackend: "other"},
			wantErr: "unknown CLAUDE_BACKEND value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearBackendEnvironment(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			got, err := configFromEnv(t.Context())
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("configFromEnv(): got nil error, want error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("configFromEnv() error: got = %q, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("configFromEnv(): got error %v, want nil", err)
			}
			if got.backend != tt.want.backend {
				t.Errorf("backend: got = %q, want = %q", got.backend, tt.want.backend)
			}
			if got.aws != tt.want.aws {
				t.Errorf("aws config: got = %#v, want = %#v", got.aws, tt.want.aws)
			}
		})
	}
}

func TestConfigFromEnvSelectsAnthropicProfile(t *testing.T) {
	tests := []struct {
		name    string
		backend string
	}{
		{name: "legacy unset behavior"},
		{name: "explicit Anthropic", backend: string(backendAnthropic)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearBackendEnvironment(t)
			dir := t.TempDir()
			configs := filepath.Join(dir, "configs")
			if err := os.MkdirAll(configs, 0o755); err != nil {
				t.Fatalf("creating Anthropic config directory: %v", err)
			}
			profile := `{
  "version": "1.0",
  "organization_id": "12345678-1234-1234-1234-123456789012",
  "authentication": {
    "type": "oidc_federation",
    "federation_rule_id": "fdrl_0123456789"
  }
}`
			if err := os.WriteFile(filepath.Join(configs, "evals.json"), []byte(profile), 0o600); err != nil {
				t.Fatalf("writing Anthropic profile: %v", err)
			}
			t.Setenv(envBackend, tt.backend)
			t.Setenv(anthropicauth.EnvProfile, "evals")
			t.Setenv(anthropicauth.EnvConfigDir, dir)

			got, err := configFromEnv(t.Context())
			if err != nil {
				t.Fatalf("configFromEnv(): got error %v, want nil", err)
			}
			if got.backend != backendAnthropic {
				t.Errorf("backend: got = %q, want = %q", got.backend, backendAnthropic)
			}
			if !got.anthropic.Configured() {
				t.Error("Anthropic config: got unconfigured, want configured profile")
			}
		})
	}
}

func TestResolveDefaultsToVertex(t *testing.T) {
	clearBackendEnvironment(t)
	setFakeADC(t)

	const model = "claude-sonnet-4-6@default"
	got, err := Resolve(t.Context(), "my-project", "us-central1", model)
	if err != nil {
		t.Fatalf("Resolve(): got error %v, want nil", err)
	}
	if got.ModelID != model {
		t.Errorf("ModelID: got = %q, want = %q", got.ModelID, model)
	}
	if got.Provider != claudeexecutor.ProviderVertex {
		t.Errorf("Provider: got = %q, want = %q", got.Provider, claudeexecutor.ProviderVertex)
	}
}

func TestResolveRejectsBrokenProfile(t *testing.T) {
	clearBackendEnvironment(t)
	t.Setenv(anthropicauth.EnvProfile, "missing")
	t.Setenv(anthropicauth.EnvConfigDir, t.TempDir())

	_, err := Resolve(t.Context(), "my-project", "us-central1", "claude-sonnet-4-6@default")
	if err == nil {
		t.Fatal("Resolve(): got = nil, want error")
	}
	if want := "resolving anthropic auth config"; !strings.Contains(err.Error(), want) {
		t.Errorf("Resolve() error: got = %q, want substring %q", err, want)
	}
}

func TestMantleModelID(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		want    string
		wantErr string
	}{
		{name: "canonical Claude ID", model: "claude-sonnet-5", want: "anthropic.claude-sonnet-5"},
		{name: "native Mantle ID", model: "anthropic.claude-sonnet-5", want: "anthropic.claude-sonnet-5"},
		{name: "Vertex version", model: "claude-sonnet-4-6@default", wantErr: "must not contain"},
		{name: "other provider", model: "gemini-3-pro", wantErr: "claude-* or anthropic.claude-* format"},
		{name: "empty", wantErr: "claude-* or anthropic.claude-* format"},
		{name: "empty Claude suffix", model: "claude-", wantErr: "claude-* or anthropic.claude-* format"},
		{name: "empty native suffix", model: "anthropic.claude-", wantErr: "claude-* or anthropic.claude-* format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mantleModelID(tt.model)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("mantleModelID(): got error %v, want nil", err)
				}
				if got != tt.want {
					t.Errorf("mantleModelID(): got = %q, want = %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatal("mantleModelID(): got nil error, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("mantleModelID() error: got = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveBedrockMantleStream(t *testing.T) {
	const (
		model       = "claude-sonnet-5"
		mantleModel = "anthropic.claude-sonnet-5"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("request path: got = %q, want = %q", r.URL.Path, "/v1/messages")
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Model != mantleModel {
			t.Errorf("request model: got = %q, want = %q", request.Model, mantleModel)
		}
		if !request.Stream {
			t.Error("request stream: got = false, want true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"id":"msg_mantle","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bedrock-ready"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		}
		var body strings.Builder
		for _, event := range events {
			var typed struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(event), &typed); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			body.WriteString("event: ")
			body.WriteString(typed.Type)
			body.WriteString("\ndata: ")
			body.WriteString(event)
			body.WriteString("\n\n")
		}
		if _, err := io.WriteString(w, body.String()); err != nil {
			t.Errorf("writing SSE response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := resolve(t.Context(), "unused-project", "unused-region", model, config{
		backend: backendBedrock,
		aws:     awsauth.Config{Region: "us-east-1", Profile: "dev-sso"},
	}, func(ctx context.Context, cfg awsauth.Config) (anthropic.MessageService, error) {
		if cfg.Region != "us-east-1" {
			t.Errorf("Region: got = %q, want = %q", cfg.Region, "us-east-1")
		}
		if cfg.Profile != "dev-sso" {
			t.Errorf("Profile: got = %q, want = %q", cfg.Profile, "dev-sso")
		}
		client, err := bedrock.NewMantleClient(ctx, bedrock.MantleClientConfig{
			AWSRegion: cfg.Region,
			BaseURL:   srv.URL,
			SkipAuth:  true,
		}, option.WithMaxRetries(0))
		if err != nil {
			return anthropic.MessageService{}, err
		}
		return client.Messages, nil
	})
	if err != nil {
		t.Fatalf("resolve(): got error %v, want nil", err)
	}
	if got.Provider != claudeexecutor.ProviderBedrock {
		t.Errorf("Provider: got = %q, want = %q", got.Provider, claudeexecutor.ProviderBedrock)
	}
	if got.ModelID != mantleModel {
		t.Errorf("ModelID: got = %q, want = %q", got.ModelID, mantleModel)
	}

	stream := got.Messages.NewStreaming(t.Context(), anthropic.MessageNewParams{
		Model:     got.ModelID,
		MaxTokens: 16,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
		},
	})
	var message anthropic.Message
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			t.Fatalf("accumulating Mantle event: %v", err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("streaming Mantle response: %v", err)
	}
	if len(message.Content) != 1 || message.Content[0].Text != "bedrock-ready" {
		t.Errorf("streamed content: got = %#v, want one bedrock-ready text block", message.Content)
	}
}

func TestResolveBedrockFailsWhenCredentialsAreUnavailable(t *testing.T) {
	want := errors.New("credentials unavailable")
	_, err := resolve(t.Context(), "unused-project", "unused-region", "claude-sonnet-4-6", config{
		backend: backendBedrock,
		aws:     awsauth.Config{Region: "us-east-1", Profile: "dev-sso"},
	}, func(context.Context, awsauth.Config) (anthropic.MessageService, error) {
		return anthropic.MessageService{}, want
	})
	if !errors.Is(err, want) {
		t.Errorf("resolve() error: got = %v, want wrapping %v", err, want)
	}
	if !strings.Contains(err.Error(), "constructing Bedrock Mantle client") {
		t.Errorf("resolve() error: got = %q, want construction context", err)
	}
}

func TestResolveBedrockFailsStartupForUnavailableProfile(t *testing.T) {
	clearBackendEnvironment(t)
	dir := t.TempDir()
	t.Setenv(envBackend, string(backendBedrock))
	t.Setenv(awsauth.EnvRegion, "us-east-1")
	t.Setenv(awsauth.EnvProfile, "missing-sso-profile")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	_, err := Resolve(t.Context(), "unused-project", "unused-region", "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("Resolve(): got nil error, want unavailable-profile error")
	}
	if !strings.Contains(err.Error(), "constructing Bedrock Mantle client") {
		t.Errorf("Resolve() error: got = %q, want construction context", err)
	}
}

func TestResolveBedrockRejectsStaticCredentialProfile(t *testing.T) {
	clearBackendEnvironment(t)
	dir := t.TempDir()
	credentialsFile := filepath.Join(dir, "credentials")
	sharedCredentials := `[static]
aws_access_key_id = placeholder-access-key
aws_secret_access_key = placeholder-secret-key
`
	if err := os.WriteFile(credentialsFile, []byte(sharedCredentials), 0o600); err != nil {
		t.Fatalf("writing AWS shared credentials: %v", err)
	}
	t.Setenv(envBackend, string(backendBedrock))
	t.Setenv(awsauth.EnvRegion, "us-west-2")
	t.Setenv(awsauth.EnvProfile, "static")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)

	_, err := Resolve(t.Context(), "unused-project", "unused-region", "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("Resolve(): got nil error, want static-profile error")
	}
	if !strings.Contains(err.Error(), "must be backed by AWS IAM Identity Center (SSO)") {
		t.Errorf("Resolve() error: got = %q, want SSO requirement", err)
	}
}

func setFakeADC(t *testing.T) {
	t.Helper()

	credFile := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(credFile, []byte(`{"type":"authorized_user","client_id":"id","client_secret":"secret","refresh_token":"token"}`), 0o600); err != nil {
		t.Fatalf("writing fake ADC file: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credFile)
}

func clearBackendEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envBackend,
		anthropicauth.EnvProfile,
		anthropicauth.EnvConfigDir,
		awsauth.EnvRegion,
		awsauth.EnvProfile,
		awsauth.EnvRoleARN,
		awsauth.EnvWebIdentityTokenFile,
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_SECURITY_TOKEN",
		"AWS_BEARER_TOKEN_BEDROCK",
		"ANTHROPIC_AWS_API_KEY",
	} {
		t.Setenv(name, "")
	}
}
