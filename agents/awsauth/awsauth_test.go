/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package awsauth

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "SSO profile",
			env: map[string]string{
				EnvRegion:  "us-east-1",
				EnvProfile: "dev-sso",
			},
			want: Config{Region: "us-east-1", Profile: "dev-sso"},
		},
		{
			name: "web identity",
			env: map[string]string{
				EnvRegion:               "us-east-2",
				EnvRoleARN:              "arn:aws:iam::123456789012:role/presubmit",
				EnvWebIdentityTokenFile: "/var/run/secrets/aws/token",
			},
			want: Config{Region: "us-east-2"},
		},
		{
			name:    "region is required",
			env:     map[string]string{EnvProfile: "dev-sso"},
			wantErr: EnvRegion,
		},
		{
			name:    "auth mode is required",
			env:     map[string]string{EnvRegion: "us-east-1"},
			wantErr: EnvProfile,
		},
		{
			name: "incomplete web identity",
			env: map[string]string{
				EnvRegion:  "us-east-1",
				EnvRoleARN: "arn:aws:iam::123456789012:role/presubmit",
			},
			wantErr: "must be set together",
		},
		{
			name: "ambiguous auth modes",
			env: map[string]string{
				EnvRegion:               "us-east-1",
				EnvProfile:              "dev-sso",
				EnvRoleARN:              "arn:aws:iam::123456789012:role/presubmit",
				EnvWebIdentityTokenFile: "/var/run/secrets/aws/token",
			},
			wantErr: "cannot be combined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnvironment(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			got, err := ConfigFromEnv(t.Context())
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("ConfigFromEnv(): got nil error, want error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("ConfigFromEnv() error: got = %q, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv(): got error %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("ConfigFromEnv(): got = %#v, want = %#v", got, tt.want)
			}
		})
	}
}

func TestConfigFromEnvRejectsForbiddenCredentials(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		wantError string
	}{
		{name: "access key ID", env: envAccessKeyID, wantError: "static credentials"},
		{name: "secret access key", env: envSecretAccessKey, wantError: "static credentials"},
		{name: "session token", env: envSessionToken, wantError: "static credentials"},
		{name: "legacy security token", env: envSecurityToken, wantError: "static credentials"},
		{name: "Bedrock bearer token", env: envBearerToken, wantError: "API-key authentication"},
		{name: "Anthropic AWS API key", env: envAnthropicAPIKey, wantError: "API-key authentication"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnvironment(t)
			t.Setenv(EnvRegion, "us-east-1")
			t.Setenv(EnvProfile, "dev-sso")
			t.Setenv(tt.env, "present")

			_, err := ConfigFromEnv(t.Context())
			if err == nil {
				t.Fatal("ConfigFromEnv(): got nil error, want error")
			}
			for _, want := range []string{tt.wantError, tt.env} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ConfigFromEnv() error: got = %q, want substring %q", err, want)
				}
			}
		})
	}
}

func TestValidateCredentials(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	credentialsFile := filepath.Join(dir, "credentials")
	sharedConfig := `[profile direct-sso]
sso_session = test
sso_account_id = 123456789012
sso_role_name = Developer
region = us-west-2

[profile assumed-sso]
role_arn = arn:aws:iam::123456789012:role/BedrockInvoker
source_profile = direct-sso
region = us-west-2

[sso-session test]
sso_start_url = https://example.awsapps.com/start
sso_region = us-east-1
sso_registration_scopes = sso:account:access
`
	if err := os.WriteFile(configFile, []byte(sharedConfig), 0o600); err != nil {
		t.Fatalf("writing AWS shared config: %v", err)
	}
	sharedCredentials := `[static]
aws_access_key_id = placeholder-access-key
aws_secret_access_key = placeholder-secret-key
`
	if err := os.WriteFile(credentialsFile, []byte(sharedCredentials), 0o600); err != nil {
		t.Fatalf("writing AWS shared credentials: %v", err)
	}

	tests := []struct {
		name        string
		profile     string
		webIdentity bool
		wantErr     string
	}{
		{name: "direct SSO profile", profile: "direct-sso"},
		{name: "assumed role sourced from SSO", profile: "assumed-sso"},
		{name: "static credentials profile", profile: "static", wantErr: "must be backed by AWS IAM Identity Center (SSO)"},
		{name: "web identity environment", webIdentity: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnvironment(t)
			t.Setenv("AWS_CONFIG_FILE", configFile)
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
			if tt.webIdentity {
				t.Setenv(EnvRoleARN, "arn:aws:iam::123456789012:role/BedrockInvoker")
				t.Setenv(EnvWebIdentityTokenFile, filepath.Join(dir, "web-identity-token"))
			}

			err := (Config{
				Region:  "us-west-2",
				Profile: tt.profile,
			}).ValidateCredentials(t.Context())
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateCredentials(): got error %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateCredentials(): got nil error, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ValidateCredentials() error: got = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadAWSConfig(t *testing.T) {
	accessKeyID := rand.Text()
	secretAccessKey := rand.Text()
	sessionToken := rand.Text()
	expiration := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	tests := []struct {
		name       string
		statusCode int
		response   string
		wantErr    string
	}{
		{
			name:       "credentials available",
			statusCode: http.StatusOK,
			response: fmt.Sprintf(`<AssumeRoleWithWebIdentityResponse>
  <AssumeRoleWithWebIdentityResult>
    <Credentials>
      <AccessKeyId>%s</AccessKeyId>
      <SecretAccessKey>%s</SecretAccessKey>
      <SessionToken>%s</SessionToken>
      <Expiration>%s</Expiration>
    </Credentials>
  </AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`, accessKeyID, secretAccessKey, sessionToken, expiration.Format(time.RFC3339)),
		},
		{
			name:       "credentials unavailable",
			statusCode: http.StatusBadRequest,
			response: `<ErrorResponse>
  <Error>
    <Type>Sender</Type>
    <Code>InvalidIdentityToken</Code>
    <Message>the web identity token was rejected</Message>
  </Error>
</ErrorResponse>`,
			wantErr: "retrieving AWS credentials",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnvironment(t)
			tokenFile := filepath.Join(t.TempDir(), "web-identity-token")
			webIdentityToken := rand.Text()
			if err := os.WriteFile(tokenFile, []byte(webIdentityToken), 0o600); err != nil {
				t.Fatalf("writing web identity token: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Method, http.MethodPost; got != want {
					t.Errorf("request method: got = %q, want = %q", got, want)
				}
				if err := r.ParseForm(); err != nil {
					t.Errorf("parsing STS request: %v", err)
				}
				for name, want := range map[string]string{
					"Action":           "AssumeRoleWithWebIdentity",
					"RoleArn":          "arn:aws:iam::123456789012:role/BedrockInvoker",
					"WebIdentityToken": webIdentityToken,
				} {
					if got := r.Form.Get(name); got != want {
						t.Errorf("request field %q: got = %q, want = %q", name, got, want)
					}
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(tt.statusCode)
				if _, err := fmt.Fprint(w, tt.response); err != nil {
					t.Errorf("writing STS response: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			t.Setenv(EnvRoleARN, "arn:aws:iam::123456789012:role/BedrockInvoker")
			t.Setenv(EnvWebIdentityTokenFile, tokenFile)
			t.Setenv("AWS_ENDPOINT_URL_STS", server.URL)

			gotConfig, err := (Config{Region: "us-west-2"}).LoadAWSConfig(t.Context())
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("LoadAWSConfig(): got nil error, want error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("LoadAWSConfig() error: got = %q, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadAWSConfig(): got error %v, want nil", err)
			}
			gotCredentials, err := gotConfig.Credentials.Retrieve(t.Context())
			if err != nil {
				t.Fatalf("Retrieve(): got error %v, want nil", err)
			}
			for name, values := range map[string][2]string{
				"access key ID":     {gotCredentials.AccessKeyID, accessKeyID},
				"secret access key": {gotCredentials.SecretAccessKey, secretAccessKey},
				"session token":     {gotCredentials.SessionToken, sessionToken},
			} {
				if got, want := values[0], values[1]; got != want {
					t.Errorf("%s: got = %q, want = %q", name, got, want)
				}
			}
			if got, want := gotCredentials.CanExpire, true; got != want {
				t.Errorf("CanExpire: got = %t, want = %t", got, want)
			}
			if got, want := gotCredentials.Expires, expiration; !got.Equal(want) {
				t.Errorf("Expires: got = %v, want = %v", got, want)
			}
		})
	}
}

func clearEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvRegion,
		EnvProfile,
		EnvRoleARN,
		EnvWebIdentityTokenFile,
		envAccessKeyID,
		envSecretAccessKey,
		envSessionToken,
		envSecurityToken,
		envBearerToken,
		envAnthropicAPIKey,
	} {
		t.Setenv(name, "")
	}
}
