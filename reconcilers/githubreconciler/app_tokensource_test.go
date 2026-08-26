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
	"testing"
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
