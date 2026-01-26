/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package apkurl provides utilities for parsing APK URL keys.
//
// APK URL keys are of the form "{host}/{repo-path...}/{arch}/{package}-{version}.apk"
// where repo-path can have multiple path components, and do NOT include the scheme
// (https://). This matches the format of the apkurl CloudEvents extension.
//
// Examples:
//   - "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk"
//   - "apk.cgr.dev/9a2552c399fb9e7ebb42c63c2c7e7984207eb31c/x86_64/glibc-2.42-r0.apk"
package apkurl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/build/types"
	"github.com/google/go-containerregistry/pkg/name"
)

// Key represents a parsed APK URL key with its components.
// Keys are of the form "{host}/{repo-path...}/{arch}/{package}-{version}.apk"
// where repo-path can have multiple path components, and do not include the
// scheme (https://).
type Key struct {
	// Host is the APK registry host (e.g., "apk.cgr.dev", "packages.wolfi.dev").
	Host string

	// RepoPath is the repository path (e.g., "os", "9a2552c399fb9e7ebb42c63c2c7e7984207eb31c").
	RepoPath string

	// Repository returns a Repository for this APK's location.
	Repository apk.Repository

	// Package contains the parsed APK package metadata.
	Package *apk.Package
}

// validAPKArchs contains the set of valid APK architecture strings,
// built from apko's types.AllArchs at init time.
var validAPKArchs map[string]struct{}

func init() {
	validAPKArchs = make(map[string]struct{}, len(types.AllArchs))
	for _, a := range types.AllArchs {
		validAPKArchs[a.ToAPK()] = struct{}{}
	}
}

// Parse parses an APK URL key into its components.
// Keys are of the form "{host}/{repo-path...}/{arch}/{package}-{version}.apk"
// where repo-path can have multiple path components, and do not include the scheme.
func Parse(key string) (*Key, error) {
	// Split into all parts
	parts := strings.Split(key, "/")

	// Minimum: host / repo-path / arch / filename = 4 parts
	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid APK key %q: expected {host}/{repo-path...}/{arch}/{pkg}.apk", key)
	}

	host := parts[0]
	if host == "" {
		return nil, fmt.Errorf("invalid APK key %q: empty host", key)
	}

	filename := parts[len(parts)-1]
	arch := parts[len(parts)-2]

	// Validate architecture against apko's supported architectures
	if _, ok := validAPKArchs[arch]; !ok {
		return nil, fmt.Errorf("invalid APK key %q: unsupported architecture %q", key, arch)
	}

	// RepoPath is everything between host and arch
	repoPathParts := parts[1 : len(parts)-2]
	if len(repoPathParts) == 0 {
		return nil, fmt.Errorf("invalid APK key %q: missing repository path", key)
	}
	repoPath := strings.Join(repoPathParts, "/")

	// Parse filename using the same logic as registry/internal/apk.parseAPKName
	pkgName, version, err := parseAPKFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("invalid APK key %q: %w", key, err)
	}

	// Build repository URI: https://{host}/{repo-path}/{arch}
	repoURI := fmt.Sprintf("https://%s/%s/%s", host, repoPath, arch)

	return &Key{
		Host:     host,
		RepoPath: repoPath,
		Repository: apk.Repository{
			URI: repoURI,
		},
		Package: &apk.Package{
			Name:    pkgName,
			Version: version,
			Arch:    arch,
		},
	}, nil
}

// parseAPKFilename extracts package name and version from APK filename.
// e.g., "kubectl-bash-completion-1.29-1.29.5-r0.apk" -> ("kubectl-bash-completion-1.29", "1.29.5-r0")
func parseAPKFilename(filename string) (pkgName, version string, err error) {
	if !strings.HasSuffix(filename, ".apk") {
		return "", "", fmt.Errorf("filename must end with .apk: %s", filename)
	}
	nameVersion := strings.TrimSuffix(filename, ".apk")

	// Find all dash positions
	var dashPositions []int
	for i, char := range nameVersion {
		if char == '-' {
			dashPositions = append(dashPositions, i)
		}
	}

	// We need at least 2 dashes for the pattern: <package>-<version>-r<revision>
	if len(dashPositions) < 2 {
		return "", "", fmt.Errorf("invalid APK filename format, expected at least 2 dashes: %s", filename)
	}

	// Split at the second dash from the end
	secondLastDashPos := dashPositions[len(dashPositions)-2]
	pkgName = nameVersion[:secondLastDashPos]
	version = nameVersion[secondLastDashPos+1:]

	if pkgName == "" || version == "" {
		return "", "", fmt.Errorf("invalid APK filename format, empty package or version: %s", filename)
	}

	return pkgName, version, nil
}

// URL returns the full HTTPS URL for fetching this APK.
func (k *Key) URL() *url.URL {
	return &url.URL{
		Scheme: "https",
		Host:   k.Host,
		Path:   fmt.Sprintf("/%s/%s/%s", k.RepoPath, k.Package.Arch, k.Package.Filename()),
	}
}

// String returns the key in its canonical form (without scheme).
func (k *Key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Host, k.RepoPath, k.Package.Arch, k.Package.Filename())
}

// FetchablePackage returns an apk.FetchablePackage for use with apko's APK client.
func (k *Key) FetchablePackage() apk.FetchablePackage {
	return apk.NewFetchablePackage(k.Package.Name, k.URL().String())
}

// StatusDigest returns a pseudo-digest reference used to store/lookup APK scan
// status via the ocistatusmanager's TargetRepository semantics.
//
// The returned reference is NOT a real OCI digest. Only the hash portion is
// meaningful; the host ("apk.cgr.dev") and repository ("__") are fixed placeholder
// values to satisfy the name.Digest type. The ocistatusmanager's TargetRepository
// setting determines where status is actually stored, and lookups are keyed solely
// by the digest hash - the host/repo have no effect on storage location.
//
// We key status off the APK control section's SHA-1 checksum (the "C:" field in
// APKINDEX and /lib/apk/db/installed) rather than the datahash for two reasons:
//  1. The control hash covers package metadata in addition to file content,
//     providing a more complete identity for the package.
//  2. The installed database only records the control checksum, not the datahash,
//     so this is the only identifier available when looking up status for packages
//     discovered in a layer's /lib/apk/db/installed file.
//
// The checksum uniquely identifies APK content regardless of which repository it
// came from, enabling status reuse across different repository paths.
//
// The pkg parameter should be the parsed APK package with its Checksum field
// populated from the control section.
func StatusDigest(pkg *apk.Package) (name.Digest, error) {
	if len(pkg.Checksum) == 0 {
		return name.Digest{}, fmt.Errorf("package %s has no checksum", pkg.Name)
	}
	checksumHex := hex.EncodeToString(pkg.Checksum)
	syntheticHash := sha256.Sum256([]byte(checksumHex))
	return name.NewDigest(fmt.Sprintf("apk.cgr.dev/__@sha256:%s", hex.EncodeToString(syntheticHash[:])))
}
