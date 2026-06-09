/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package anthropicauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGithubActionsIDToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer reqtok"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("audience"); got != identityTokenAudience {
			t.Errorf("audience = %q, want %q", got, identityTokenAudience)
		}
		w.Write([]byte(`{"value":"jwt-fresh"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	cfg := Config{
		// The runner's real URL already carries a query string; mimic that.
		ActionsIDTokenRequestURL:   srv.URL + "/token?api-version=2",
		ActionsIDTokenRequestToken: "reqtok",
	}
	got, err := githubActionsIDToken(cfg)(t.Context())
	if err != nil {
		t.Fatalf("githubActionsIDToken() error = %v", err)
	}
	if got != "jwt-fresh" {
		t.Errorf("githubActionsIDToken() = %q, want %q", got, "jwt-fresh")
	}
}

func TestGithubActionsIDTokenErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "non-200",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr: "returned 500",
		},
		{
			name: "empty value",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(`{"value":""}`)) //nolint:errcheck
			},
			wantErr: "empty value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			cfg := Config{
				ActionsIDTokenRequestURL:   srv.URL + "/token?api-version=2",
				ActionsIDTokenRequestToken: "reqtok",
			}
			_, err := githubActionsIDToken(cfg)(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("githubActionsIDToken() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
