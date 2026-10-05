/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
	"github.com/google/go-github/v88/github"
)

// An App built with WithInstallationPermissions asks for exactly those
// permissions on every mint and refuses a token GitHub grants any other
// set; without it, a token carries whatever the installation grants.
func TestWithInstallationPermissions(t *testing.T) {
	narrow := github.InstallationPermissions{
		Contents:     new("read"),
		Metadata:     new("read"),
		PullRequests: new("write"),
	}
	for _, tc := range []struct {
		name    string
		opts    []AppOption
		grant   map[string]string
		wantErr bool
	}{{
		name: "no option takes what the installation grants",
	}, {
		name: "granted as requested",
		opts: []AppOption{WithInstallationPermissions(narrow)},
	}, {
		name: "metadata read is added",
		opts: []AppOption{WithInstallationPermissions(github.InstallationPermissions{
			Contents:     new("read"),
			PullRequests: new("write"),
		})},
		grant: map[string]string{"contents": "read", "metadata": "read", "pull_requests": "write"},
	}, {
		name:    "granted more than requested",
		opts:    []AppOption{WithInstallationPermissions(narrow)},
		grant:   fakeAppPermissions,
		wantErr: true,
	}, {
		name:    "granted less than requested",
		opts:    []AppOption{WithInstallationPermissions(narrow)},
		grant:   map[string]string{"contents": "read", "metadata": "read"},
		wantErr: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGitHub(map[string]int64{"acme": 42})
			fake.grant = tc.grant
			srv := httptest.NewServer(fake)
			t.Cleanup(srv.Close)
			app := newTestApp(t, srv.Client().Transport, srv.URL, tc.opts...)

			// Both the repository-scoped and the org-wide mint.
			for _, repo := range []string{"widgets", ""} {
				ts, err := app.RepoTokenSource(t.Context(), "acme", repo)
				if err != nil {
					t.Fatalf("RepoTokenSource(%q): %v", repo, err)
				}
				tok, err := ts.Token()
				if gotErr := err != nil; gotErr != tc.wantErr {
					t.Fatalf("Token(%q): got err = %v, want error = %t", repo, err, tc.wantErr)
				}
				if err == nil && tok.AccessToken == "" {
					t.Errorf("Token(%q): empty token", repo)
				}
			}
		})
	}
}

func TestNewAppFromFile(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	const appID = 12345
	app, err := NewApp(t.Context(), appID, "file://"+keyPath)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if got := app.ID(); got != appID {
		t.Errorf("ID(): got = %d, want = %d", got, appID)
	}
}

func TestNewAppBadKeyURI(t *testing.T) {
	for _, uri := range []string{
		"",
		"/no/scheme.pem",
		"vault://secret/github-app",
		"file:///does/not/exist.pem",
	} {
		t.Run(uri, func(t *testing.T) {
			if _, err := NewApp(t.Context(), 1, uri); err == nil {
				t.Errorf("NewApp(%q): got nil error, want error", uri)
			}
		})
	}
}

func TestAppTokenSource(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	const appID int64 = 210473
	app, err := NewApp(t.Context(), appID, "file://"+keyPath)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	ts := app.AppTokenSource()
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, want Bearer", tok.TokenType)
	}
	if !tok.Expiry.After(time.Now()) {
		t.Errorf("Expiry = %v, want in the future", tok.Expiry)
	}

	// The JWT verifies against the App key and is issued by the App.
	var claims jwt.RegisteredClaims
	parsed, err := jwt.ParseWithClaims(tok.AccessToken, &claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil })
	if err != nil || !parsed.Valid {
		t.Fatalf("parsing JWT: valid=%v err=%v", parsed != nil && parsed.Valid, err)
	}
	if got, want := claims.Issuer, strconv.FormatInt(appID, 10); got != want {
		t.Errorf("iss = %q, want %q", got, want)
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || !claims.IssuedAt.Before(time.Now()) {
		t.Errorf("iat/exp = %v/%v, want iat in the past and exp set", claims.IssuedAt, claims.ExpiresAt)
	}

	// Valid tokens are reused rather than re-signed on every call.
	again, err := ts.Token()
	if err != nil {
		t.Fatalf("Token (again): %v", err)
	}
	if again.AccessToken != tok.AccessToken {
		t.Error("second Token() re-signed a still-valid JWT; want reuse")
	}
}
