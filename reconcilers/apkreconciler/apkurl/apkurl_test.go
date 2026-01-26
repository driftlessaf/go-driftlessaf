/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package apkurl

import (
	"encoding/hex"
	"testing"

	"chainguard.dev/apko/pkg/apk/apk"
)

func TestParse(t *testing.T) {
	// Valid UIDP components for testing:
	// Root UIDP: 40 hex chars
	// Child segment: 16 hex chars
	const (
		validRoot   = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
		validChild1 = "1234567890abcdef"
		validChild2 = "fedcba0987654321"
	)

	tests := []struct {
		name         string
		key          string
		wantHost     string
		wantRepoPath string
		wantArch     string
		wantPkg      string
		wantVersion  string
		wantRepoURI  string
		wantErr      bool
	}{
		// Wolfi-style paths (friendly names)
		{
			name:         "wolfi os repository",
			key:          "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk",
			wantHost:     "packages.wolfi.dev",
			wantRepoPath: "os",
			wantArch:     "x86_64",
			wantPkg:      "glibc",
			wantVersion:  "2.42-r0",
			wantRepoURI:  "https://packages.wolfi.dev/os/x86_64",
		},
		{
			name:         "wolfi aarch64",
			key:          "packages.wolfi.dev/os/aarch64/busybox-1.36.1-r0.apk",
			wantHost:     "packages.wolfi.dev",
			wantRepoPath: "os",
			wantArch:     "aarch64",
			wantPkg:      "busybox",
			wantVersion:  "1.36.1-r0",
			wantRepoURI:  "https://packages.wolfi.dev/os/aarch64",
		},
		{
			name:         "friendly name repo path",
			key:          "apk.cgr.dev/chainguard/x86_64/glibc-2.42-r0.apk",
			wantHost:     "apk.cgr.dev",
			wantRepoPath: "chainguard",
			wantArch:     "x86_64",
			wantPkg:      "glibc",
			wantVersion:  "2.42-r0",
			wantRepoURI:  "https://apk.cgr.dev/chainguard/x86_64",
		},
		// UIDP-style paths (for private registries)
		{
			name:         "UIDP (root only)",
			key:          "apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.apk",
			wantHost:     "apk.cgr.dev",
			wantRepoPath: validRoot,
			wantArch:     "x86_64",
			wantPkg:      "glibc",
			wantVersion:  "2.42-r0",
			wantRepoURI:  "https://apk.cgr.dev/" + validRoot + "/x86_64",
		},
		{
			name:         "UIDP with one child",
			key:          "apk.cgr.dev/" + validRoot + "/" + validChild1 + "/x86_64/glibc-2.42-r0.apk",
			wantHost:     "apk.cgr.dev",
			wantRepoPath: validRoot + "/" + validChild1,
			wantArch:     "x86_64",
			wantPkg:      "glibc",
			wantVersion:  "2.42-r0",
			wantRepoURI:  "https://apk.cgr.dev/" + validRoot + "/" + validChild1 + "/x86_64",
		},
		{
			name:         "UIDP with two children",
			key:          "apk.cgr.dev/" + validRoot + "/" + validChild1 + "/" + validChild2 + "/aarch64/openssl-3.1.0-r5.apk",
			wantHost:     "apk.cgr.dev",
			wantRepoPath: validRoot + "/" + validChild1 + "/" + validChild2,
			wantArch:     "aarch64",
			wantPkg:      "openssl",
			wantVersion:  "3.1.0-r5",
			wantRepoURI:  "https://apk.cgr.dev/" + validRoot + "/" + validChild1 + "/" + validChild2 + "/aarch64",
		},
		// Package name edge cases
		{
			name:         "package name with dashes",
			key:          "apk.cgr.dev/" + validRoot + "/x86_64/kubectl-bash-completion-1.29.5-r0.apk",
			wantHost:     "apk.cgr.dev",
			wantRepoPath: validRoot,
			wantArch:     "x86_64",
			wantPkg:      "kubectl-bash-completion",
			wantVersion:  "1.29.5-r0",
			wantRepoURI:  "https://apk.cgr.dev/" + validRoot + "/x86_64",
		},
		{
			name:         "package name with many dashes",
			key:          "apk.cgr.dev/" + validRoot + "/x86_64/some-very-long-package-name-1.0.0-r0.apk",
			wantHost:     "apk.cgr.dev",
			wantRepoPath: validRoot,
			wantArch:     "x86_64",
			wantPkg:      "some-very-long-package-name",
			wantVersion:  "1.0.0-r0",
			wantRepoURI:  "https://apk.cgr.dev/" + validRoot + "/x86_64",
		},
		{
			name:         "version with epoch",
			key:          "apk.cgr.dev/" + validRoot + "/x86_64/python-3.11-3.11.8-r0.apk",
			wantHost:     "apk.cgr.dev",
			wantRepoPath: validRoot,
			wantArch:     "x86_64",
			wantPkg:      "python-3.11",
			wantVersion:  "3.11.8-r0",
			wantRepoURI:  "https://apk.cgr.dev/" + validRoot + "/x86_64",
		},
		// Error cases
		{
			name:    "empty key",
			key:     "",
			wantErr: true,
		},
		{
			name:    "only host",
			key:     "apk.cgr.dev",
			wantErr: true,
		},
		{
			name:    "missing filename",
			key:     "apk.cgr.dev/" + validRoot + "/x86_64",
			wantErr: true,
		},
		{
			name:    "missing arch",
			key:     "apk.cgr.dev/" + validRoot + "/glibc-2.42-r0.apk",
			wantErr: true,
		},
		{
			name:    "not an apk file",
			key:     "apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.tar.gz",
			wantErr: true,
		},
		{
			name:    "missing version dashes",
			key:     "apk.cgr.dev/" + validRoot + "/x86_64/glibc.apk",
			wantErr: true,
		},
		{
			name:    "only one dash in filename",
			key:     "apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42.apk",
			wantErr: true,
		},
		{
			name:    "empty host with path",
			key:     "/" + validRoot + "/x86_64/glibc-2.42-r0.apk",
			wantErr: true,
		},
		{
			name:    "trailing slash",
			key:     "apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.apk/",
			wantErr: true,
		},
		{
			name:    "invalid architecture",
			key:     "apk.cgr.dev/" + validRoot + "/invalid_arch/glibc-2.42-r0.apk",
			wantErr: true,
		},
		{
			name:    "unknown architecture i386",
			key:     "apk.cgr.dev/" + validRoot + "/i386/glibc-2.42-r0.apk",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if got.Host != tt.wantHost {
				t.Errorf("Parse() Host = %q, want %q", got.Host, tt.wantHost)
			}
			if got.RepoPath != tt.wantRepoPath {
				t.Errorf("Parse() RepoPath = %q, want %q", got.RepoPath, tt.wantRepoPath)
			}
			if got.Package.Arch != tt.wantArch {
				t.Errorf("Parse() Arch = %q, want %q", got.Package.Arch, tt.wantArch)
			}
			if got.Package.Name != tt.wantPkg {
				t.Errorf("Parse() Package.Name = %q, want %q", got.Package.Name, tt.wantPkg)
			}
			if got.Package.Version != tt.wantVersion {
				t.Errorf("Parse() Package.Version = %q, want %q", got.Package.Version, tt.wantVersion)
			}
			if got.Repository.URI != tt.wantRepoURI {
				t.Errorf("Parse() Repository.URI = %q, want %q", got.Repository.URI, tt.wantRepoURI)
			}
		})
	}
}

func TestParseAPKFilename(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		wantPkg     string
		wantVersion string
		wantErr     bool
	}{
		{
			name:        "simple package",
			filename:    "glibc-2.42-r0.apk",
			wantPkg:     "glibc",
			wantVersion: "2.42-r0",
		},
		{
			name:        "package with dash in name",
			filename:    "go-tools-0.18.0-r0.apk",
			wantPkg:     "go-tools",
			wantVersion: "0.18.0-r0",
		},
		{
			name:        "package with multiple dashes",
			filename:    "kubectl-bash-completion-1.29.5-r0.apk",
			wantPkg:     "kubectl-bash-completion",
			wantVersion: "1.29.5-r0",
		},
		{
			name:        "version number in package name",
			filename:    "python-3.11-3.11.8-r0.apk",
			wantPkg:     "python-3.11",
			wantVersion: "3.11.8-r0",
		},
		{
			name:        "complex version",
			filename:    "openssl-3.1.4-r5.apk",
			wantPkg:     "openssl",
			wantVersion: "3.1.4-r5",
		},
		{
			name:        "high revision number",
			filename:    "busybox-1.36.1-r100.apk",
			wantPkg:     "busybox",
			wantVersion: "1.36.1-r100",
		},
		// Error cases
		{
			name:     "not apk extension",
			filename: "glibc-2.42-r0.tar.gz",
			wantErr:  true,
		},
		{
			name:     "no extension",
			filename: "glibc-2.42-r0",
			wantErr:  true,
		},
		{
			name:     "no dashes",
			filename: "glibc.apk",
			wantErr:  true,
		},
		{
			name:     "only one dash",
			filename: "glibc-2.42.apk",
			wantErr:  true,
		},
		{
			name:     "empty filename",
			filename: "",
			wantErr:  true,
		},
		{
			name:     "just .apk",
			filename: ".apk",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPkg, gotVersion, err := parseAPKFilename(tt.filename)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseAPKFilename() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if gotPkg != tt.wantPkg {
				t.Errorf("parseAPKFilename() pkg = %q, want %q", gotPkg, tt.wantPkg)
			}
			if gotVersion != tt.wantVersion {
				t.Errorf("parseAPKFilename() version = %q, want %q", gotVersion, tt.wantVersion)
			}
		})
	}
}

func TestKey_URL(t *testing.T) {
	const (
		validRoot  = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
		validChild = "1234567890abcdef"
	)

	tests := []struct {
		name    string
		key     string
		wantURL string
	}{
		{
			name:    "wolfi",
			key:     "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk",
			wantURL: "https://packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk",
		},
		{
			name:    "UIDP",
			key:     "apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.apk",
			wantURL: "https://apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.apk",
		},
		{
			name:    "multi-part repo path",
			key:     "apk.cgr.dev/" + validRoot + "/" + validChild + "/x86_64/glibc-2.42-r0.apk",
			wantURL: "https://apk.cgr.dev/" + validRoot + "/" + validChild + "/x86_64/glibc-2.42-r0.apk",
		},
		{
			name:    "aarch64",
			key:     "packages.wolfi.dev/os/aarch64/busybox-1.36.1-r0.apk",
			wantURL: "https://packages.wolfi.dev/os/aarch64/busybox-1.36.1-r0.apk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := Parse(tt.key)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			gotURL := k.URL().String()
			if gotURL != tt.wantURL {
				t.Errorf("URL() = %q, want %q", gotURL, tt.wantURL)
			}
		})
	}
}

func TestKey_String(t *testing.T) {
	const (
		validRoot  = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
		validChild = "1234567890abcdef"
	)

	tests := []struct {
		name string
		key  string
	}{
		{
			name: "wolfi",
			key:  "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk",
		},
		{
			name: "UIDP",
			key:  "apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.apk",
		},
		{
			name: "multi-part repo path",
			key:  "apk.cgr.dev/" + validRoot + "/" + validChild + "/x86_64/glibc-2.42-r0.apk",
		},
		{
			name: "complex package name",
			key:  "packages.wolfi.dev/os/x86_64/kubectl-bash-completion-1.29.5-r0.apk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := Parse(tt.key)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			// String() should return the original key
			gotStr := k.String()
			if gotStr != tt.key {
				t.Errorf("String() = %q, want %q", gotStr, tt.key)
			}
		})
	}
}

func TestKey_FetchablePackage(t *testing.T) {
	key := "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk"
	k, err := Parse(key)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	fp := k.FetchablePackage()

	wantName := "glibc"
	if fp.PackageName() != wantName {
		t.Errorf("FetchablePackage().PackageName() = %q, want %q", fp.PackageName(), wantName)
	}

	wantURL := "https://packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk"
	if fp.URL() != wantURL {
		t.Errorf("FetchablePackage().URL() = %q, want %q", fp.URL(), wantURL)
	}
}

func TestKey_PackageFilename(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		wantFilename string
	}{
		{
			name:         "simple",
			key:          "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk",
			wantFilename: "glibc-2.42-r0.apk",
		},
		{
			name:         "complex package",
			key:          "packages.wolfi.dev/os/x86_64/kubectl-bash-completion-1.29.5-r0.apk",
			wantFilename: "kubectl-bash-completion-1.29.5-r0.apk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := Parse(tt.key)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			// Package.Filename() uses apko's format
			gotFilename := k.Package.Filename()
			if gotFilename != tt.wantFilename {
				t.Errorf("Package.Filename() = %q, want %q", gotFilename, tt.wantFilename)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	const (
		validRoot   = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
		validChild1 = "1234567890abcdef"
		validChild2 = "fedcba0987654321"
	)

	// Test that parsing and then String() returns the original key
	keys := []string{
		// Wolfi-style paths
		"packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk",
		"packages.wolfi.dev/os/aarch64/openssl-3.1.0-r5.apk",
		// UIDP-style paths
		"apk.cgr.dev/" + validRoot + "/x86_64/glibc-2.42-r0.apk",
		"apk.cgr.dev/" + validRoot + "/" + validChild1 + "/x86_64/glibc-2.42-r0.apk",
		"apk.cgr.dev/" + validRoot + "/" + validChild1 + "/" + validChild2 + "/aarch64/openssl-3.1.0-r5.apk",
		// Mixed
		"apk.cgr.dev/chainguard/x86_64/curl-8.5.0-r0.apk",
		"packages.wolfi.dev/os/x86_64/python-3.11-3.11.8-r0.apk",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			k, err := Parse(key)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			roundTripped := k.String()
			if roundTripped != key {
				t.Errorf("Round trip failed: got %q, want %q", roundTripped, key)
			}
		})
	}
}

func TestStatusDigest(t *testing.T) {
	// Example SHA-1 checksum (20 bytes) from an APK control section
	checksumHex := "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	checksum, err := hex.DecodeString(checksumHex)
	if err != nil {
		t.Fatalf("failed to decode test checksum: %v", err)
	}

	tests := []struct {
		name    string
		pkg     *apk.Package
		wantErr bool
	}{{
		name: "valid checksum",
		pkg: &apk.Package{
			Name:     "glibc",
			Checksum: checksum,
		},
	}, {
		name: "empty checksum",
		pkg: &apk.Package{
			Name: "glibc",
			// No checksum
		},
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			digest, err := StatusDigest(tt.pkg)
			if (err != nil) != tt.wantErr {
				t.Errorf("StatusDigest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			// Verify the digest uses the fixed placeholder host
			if digest.RegistryStr() != "apk.cgr.dev" {
				t.Errorf("StatusDigest() registry = %q, want %q", digest.RegistryStr(), "apk.cgr.dev")
			}

			// Verify the repo path is the placeholder "__"
			if digest.RepositoryStr() != "__" {
				t.Errorf("StatusDigest() repository = %q, want %q", digest.RepositoryStr(), "__")
			}

			// Verify the digest is a valid SHA-256 digest
			digestStr := digest.DigestStr()
			if len(digestStr) != 71 { // "sha256:" + 64 hex chars
				t.Errorf("StatusDigest() digest length = %d, want 71", len(digestStr))
			}
		})
	}
}

func TestStatusDigest_Deterministic(t *testing.T) {
	// Verify that the same checksum always produces the same digest
	checksumHex := "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	checksum, err := hex.DecodeString(checksumHex)
	if err != nil {
		t.Fatalf("failed to decode test checksum: %v", err)
	}

	pkg := &apk.Package{
		Name:     "glibc",
		Checksum: checksum,
	}

	digest1, err := StatusDigest(pkg)
	if err != nil {
		t.Fatalf("first StatusDigest() error = %v", err)
	}

	digest2, err := StatusDigest(pkg)
	if err != nil {
		t.Fatalf("second StatusDigest() error = %v", err)
	}

	if digest1.String() != digest2.String() {
		t.Errorf("StatusDigest() not deterministic: %q != %q", digest1.String(), digest2.String())
	}
}
