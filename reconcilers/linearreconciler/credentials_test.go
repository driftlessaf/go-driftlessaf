/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCredentialsNewClient(t *testing.T) {
	tests := []struct {
		name       string
		creds      Credentials
		scopes     []string
		wantAPIKey bool
		wantScopes []string
		wantErr    string
	}{
		{name: "api key", creds: Credentials{APIKey: "lin_api_x"}, wantAPIKey: true},
		{name: "oauth default scopes", creds: Credentials{ClientID: "id", ClientSecret: "secret"}, wantScopes: []string{ScopeRead, ScopeWrite}},
		{name: "oauth custom scopes", creds: Credentials{ClientID: "id", ClientSecret: "secret"}, scopes: []string{ScopeRead, ScopeWrite, ScopeCommentsCreate}, wantScopes: []string{ScopeRead, ScopeWrite, ScopeCommentsCreate}},
		{name: "scopes ignored for api key", creds: Credentials{APIKey: "lin_api_x"}, scopes: []string{ScopeRead}, wantAPIKey: true},
		{name: "both forms", creds: Credentials{APIKey: "lin_api_x", ClientID: "id", ClientSecret: "secret"}, wantErr: "not both"},
		{name: "half pair", creds: Credentials{ClientID: "id"}, wantErr: "together"},
		{name: "nothing", wantErr: "no Linear credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := tt.creds.NewClient(tt.scopes...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error: got = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.isAPIKey != tt.wantAPIKey {
				t.Errorf("isAPIKey: got = %t, want = %t", c.isAPIKey, tt.wantAPIKey)
			}
			if diff := cmp.Diff(tt.wantScopes, c.scopes); diff != "" {
				t.Errorf("scopes (-want, +got):\n%s", diff)
			}
		})
	}
}
