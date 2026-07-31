/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package apkurl

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func randomChecksum(t *testing.T, size int) []byte {
	t.Helper()
	sum := make([]byte, size)
	if _, err := rand.Read(sum); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return sum
}

func TestParseChecksum(t *testing.T) {
	const base = "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk"
	sha1sum := randomChecksum(t, 20)
	sha256sum := randomChecksum(t, 32)

	tests := []struct {
		name         string
		key          string
		wantChecksum []byte
		wantErr      string
	}{{
		name:    "no suffix",
		key:     base,
		wantErr: "missing required control-checksum suffix",
	}, {
		name:         "sha1 suffix",
		key:          base + "@sha1:" + hex.EncodeToString(sha1sum),
		wantChecksum: sha1sum,
	}, {
		name:         "sha256 suffix",
		key:          base + "@sha256:" + hex.EncodeToString(sha256sum),
		wantChecksum: sha256sum,
	}, {
		name:    "missing colon",
		key:     base + "@sha1",
		wantErr: "not {alg}:{hex}",
	}, {
		name:    "unsupported algorithm",
		key:     base + "@md5:" + hex.EncodeToString(sha1sum),
		wantErr: "unsupported checksum algorithm",
	}, {
		name:    "invalid hex",
		key:     base + "@sha1:zzzz",
		wantErr: "decoding checksum",
	}, {
		name:    "wrong length for algorithm",
		key:     base + "@sha1:" + hex.EncodeToString(sha256sum),
		wantErr: "must be 20 bytes",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := Parse(tt.key)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse() error: got = nil, want containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse() error: got = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if !bytes.Equal(k.Package.Checksum, tt.wantChecksum) {
				t.Errorf("checksum: got = %x, want = %x", k.Package.Checksum, tt.wantChecksum)
			}
			// The checksum never leaks into the fetch URL.
			if got := k.URL().String(); strings.Contains(got, "@") {
				t.Errorf("URL(): got = %q, want no checksum suffix", got)
			}
		})
	}
}

func TestRoundTripChecksum(t *testing.T) {
	sha1sum := randomChecksum(t, 20)
	sha256sum := randomChecksum(t, 32)
	keys := []string{
		"packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk@sha1:" + hex.EncodeToString(sha1sum),
		"apk.cgr.dev/a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2/aarch64/openssl-3.1.0-r5.apk@sha256:" + hex.EncodeToString(sha256sum),
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			k, err := Parse(key)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got := k.String(); got != key {
				t.Errorf("round trip: got = %q, want = %q", got, key)
			}
		})
	}
}

func TestStringOmitsUnrecognizedChecksum(t *testing.T) {
	key, err := Parse("packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk@sha1:" + hex.EncodeToString(randomChecksum(t, 20)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	key.Package.Checksum = randomChecksum(t, 7)
	if got := key.String(); strings.Contains(got, "@") {
		t.Errorf("String(): got = %q, want legacy form for unrecognized checksum length", got)
	}
}

func TestStatusDigestFromPinnedKey(t *testing.T) {
	// A digest derived from a parsed pinned key must match one derived from
	// a package whose checksum was read out of the fetched control section.
	sum := randomChecksum(t, 20)
	pinned, err := Parse("packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk@sha1:" + hex.EncodeToString(sum))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	fromKey, err := pinned.StatusDigest()
	if err != nil {
		t.Fatalf("Key.StatusDigest() error = %v", err)
	}
	fromFetched, err := StatusDigest(sum)
	if err != nil {
		t.Fatalf("StatusDigest(checksum) error = %v", err)
	}
	if fromKey.String() != fromFetched.String() {
		t.Errorf("status digest: got = %v, want = %v", fromKey, fromFetched)
	}
}

func TestParseMissingChecksumSentinel(t *testing.T) {
	_, err := Parse("packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk")
	if !errors.Is(err, ErrMissingChecksum) {
		t.Errorf("errors.Is(err, ErrMissingChecksum): got = false (err=%v), want = true", err)
	}
}
