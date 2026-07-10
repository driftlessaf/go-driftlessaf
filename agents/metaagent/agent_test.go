/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

type testRequest struct{}

func (r *testRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type testResponse struct{}

// testCallbacks is the standard tool composition: Empty -> Worktree -> Finding
type testCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

func TestNewModelSelection(t *testing.T) {
	config := Config[*testResponse, testCallbacks]{
		Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
			toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
				toolcall.NewEmptyToolsProvider[*testResponse]())),
	}

	tests := []struct {
		name    string
		model   string
		wantErr string
	}{{
		name:    "unsupported model",
		model:   "unknown-model",
		wantErr: "unsupported model",
	}, {
		name:    "empty model",
		model:   "",
		wantErr: "unsupported model",
	}, {
		name:    "partial gemini",
		model:   "gem",
		wantErr: "unsupported model",
	}, {
		name:    "partial claude",
		model:   "cla",
		wantErr: "unsupported model",
	}, {
		name:    "slash routes to openai compat",
		model:   "google/gemini-2.5-pro",
		wantErr: "prompt cannot be nil",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New[*testRequest](t.Context(), "test-project", "us-central1", tt.model, config)
			if err == nil {
				t.Errorf("New() got = nil, want error")
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New() got = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestUserPromptSuffixAcceptedOnOpenAIBackend pins the config-compat contract
// of the OpenAI-compatible path: the suffix is folded into the built user
// prompt (see openaiexecutor.WithUserPromptSuffix), so setting one must not
// change whether construction succeeds. The model is operator-configurable
// (e.g. REVIEWER_MODEL), so a suffix-specific rejection here would break
// deployments pointing at a publisher/model value that worked before.
//
// Construction acquires Application Default Credentials before applying
// executor options, so the test points GOOGLE_APPLICATION_CREDENTIALS at a
// hermetic fake service-account key. That makes credential acquisition
// succeed deterministically — in CI and locally alike — and construction
// genuinely reaches option application with the suffix set, rather than
// failing early and passing this test vacuously.
func TestUserPromptSuffixAcceptedOnOpenAIBackend(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))

	userPrompt, err := promptbuilder.NewPrompt("payload")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	suffix, err := promptbuilder.NewPrompt("lens suffix")
	if err != nil {
		t.Fatalf("NewPrompt(suffix) error = %v", err)
	}

	config := Config[*testResponse, testCallbacks]{
		UserPrompt: userPrompt,
		Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
			toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
				toolcall.NewEmptyToolsProvider[*testResponse]())),
	}

	if _, err := New[*testRequest](t.Context(), "test-project", "us-central1", "google/gemini-2.5-pro", config); err != nil {
		t.Fatalf("New() without suffix: got = %v, want = nil", err)
	}

	config.UserPromptSuffix = suffix
	if _, err := New[*testRequest](t.Context(), "test-project", "us-central1", "google/gemini-2.5-pro", config); err != nil {
		t.Errorf("New() with suffix: got = %v, want = nil", err)
	}
}

// fakeGoogleCredentials writes a syntactically valid service-account key with
// a freshly generated throwaway RSA key to a temp file and returns its path.
// Application Default Credentials parses the file without any network calls,
// so pointing GOOGLE_APPLICATION_CREDENTIALS at it makes credential
// acquisition hermetic.
func fakeGoogleCredentials(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating throwaway key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	creds, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": "fake-key-id",
		"private_key":    string(keyPEM),
		"client_email":   "fake@test-project.iam.gserviceaccount.com",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshaling fake credentials: %v", err)
	}

	path := filepath.Join(t.TempDir(), "fake-credentials.json")
	if err := os.WriteFile(path, creds, 0o600); err != nil {
		t.Fatalf("writing fake credentials: %v", err)
	}
	return path
}
