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
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
)

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
