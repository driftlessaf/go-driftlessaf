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
// Keys carry a required "@{alg}:{hex}" suffix pinning the APK's control
// section checksum (the "C:" field in APKINDEX and /lib/apk/db/installed),
// mirroring how image keys pin a digest. The pinned checksum is the APK's
// identity: it lets consumers derive the status identity (see StatusDigest)
// without fetching the APK, and Parse rejects keys without it.
//
// Examples:
//   - "packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk@sha1:9a378af1ca9dc9afeb27acefee7953b8c6fcda1d"
//   - "apk.cgr.dev/9a2552c399fb9e7ebb42c63c2c7e7984207eb31c/x86_64/glibc-2.42-r0.apk@sha1:9a378af1ca9dc9afeb27acefee7953b8c6fcda1d"
package apkurl

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/build/types"
	"github.com/google/go-containerregistry/pkg/name"
)

// Reference is a parsed APK location with an optional control checksum.
// References use the same qualified path grammar as Key, but may omit the
// checksum while a caller resolves it through an APKINDEX.
type Reference struct {
	// Host is the APK registry host (e.g., "apk.cgr.dev", "packages.wolfi.dev").
	Host string

	// RepoPath is the repository path (e.g., "os", "9a2552c399fb9e7ebb42c63c2c7e7984207eb31c").
	RepoPath string

	// Repository is this APK's repository and architecture.
	Repository apk.Repository

	// Package contains the parsed APK package metadata and optional checksum.
	Package *apk.Package

	// ChecksumAlgorithm names the checksum algorithm when Package.Checksum is
	// present. It is empty for an unpinned reference.
	ChecksumAlgorithm string
}

// Key represents a checksum-pinned APK URL key with its components.
// Keys are of the form "{host}/{repo-path...}/{arch}/{package}-{version}.apk"
// where repo-path can have multiple path components, and do not include the
// scheme (https://).
type Key struct {
	// Host is the APK registry host (e.g., "apk.cgr.dev", "packages.wolfi.dev").
	Host string

	// RepoPath is the repository path (e.g., "os", "9a2552c399fb9e7ebb42c63c2c7e7984207eb31c").
	RepoPath string

	// Repository is this APK's repository and architecture.
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

// Checksum suffix algorithms, keyed by the byte length of the decoded sum.
// APK control checksums are SHA-1 ("Q1" entries in APKINDEX); SHA-256 ("Q2")
// is recognized for forward compatibility.
const (
	sha1Size   = 20
	sha256Size = 32
)

var checksumSizes = map[string]int{
	"sha1":   sha1Size,
	"sha256": sha256Size,
}

// ErrMissingChecksum reports an APK key lacking the required "@{alg}:{hex}"
// control-checksum suffix. Callers can detect it with errors.Is to
// distinguish legacy-form keys from other malformations.
var ErrMissingChecksum = errors.New(`missing required control-checksum suffix ("@{alg}:{hex}")`)

// SplitChecksum splits an optional "@{alg}:{hex}" control-checksum suffix
// from a reference. Algorithm and checksum are empty when no suffix is
// present.
func SplitChecksum(ref string) (rest, algorithm string, checksum []byte, err error) {
	rest, suffix, found := strings.Cut(ref, "@")
	if !found {
		return ref, "", nil, nil
	}
	algorithm, hexsum, found := strings.Cut(suffix, ":")
	if !found {
		return "", "", nil, fmt.Errorf("invalid APK key %q: checksum suffix %q is not {alg}:{hex}", ref, suffix)
	}
	size, ok := checksumSizes[algorithm]
	if !ok {
		return "", "", nil, fmt.Errorf("invalid APK key %q: unsupported checksum algorithm %q", ref, algorithm)
	}
	checksum, err = hex.DecodeString(hexsum)
	if err != nil {
		return "", "", nil, fmt.Errorf("invalid APK key %q: decoding checksum: %w", ref, err)
	}
	if len(checksum) != size {
		return "", "", nil, fmt.Errorf("invalid APK key %q: %s checksum must be %d bytes, got %d", ref, algorithm, size, len(checksum))
	}
	return rest, algorithm, checksum, nil
}

// ParseReference parses a qualified APK reference with an optional control
// checksum. References use the form
// "{host}/{repo-path...}/{arch}/{package}-{version}.apk[@{alg}:{hex}]" and do
// not include a URL scheme.
func ParseReference(ref string) (*Reference, error) {
	key, algorithm, checksum, err := SplitChecksum(ref)
	if err != nil {
		return nil, err
	}
	return parseReference(key, algorithm, checksum)
}

// Parse parses an APK URL key into its components.
// Keys are of the form "{host}/{repo-path...}/{arch}/{package}-{version}.apk"
// with a required "@{alg}:{hex}" control-checksum suffix; they do not
// include the scheme.
func Parse(key string) (*Key, error) {
	unpinned, algorithm, checksum, err := SplitChecksum(key)
	if err != nil {
		return nil, err
	}
	if len(checksum) == 0 {
		return nil, fmt.Errorf("invalid APK key %q: %w", unpinned, ErrMissingChecksum)
	}

	ref, err := parseReference(unpinned, algorithm, checksum)
	if err != nil {
		return nil, err
	}
	return &Key{
		Host:       ref.Host,
		RepoPath:   ref.RepoPath,
		Repository: ref.Repository,
		Package:    ref.Package,
	}, nil
}

func parseReference(key, algorithm string, checksum []byte) (*Reference, error) {
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

	return &Reference{
		Host:     host,
		RepoPath: repoPath,
		Repository: apk.Repository{
			URI: repoURI,
		},
		Package: &apk.Package{
			Name:     pkgName,
			Version:  version,
			Arch:     arch,
			Checksum: checksum,
		},
		ChecksumAlgorithm: algorithm,
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

// String returns the key in its canonical form (without scheme). When the
// package carries a control checksum of a recognized algorithm, it is pinned
// as an "@{alg}:{hex}" suffix so consumers can derive the status identity
// without fetching the APK; unrecognized checksum shapes are omitted rather
// than emitting a suffix Parse would reject.
func (k *Key) String() string {
	base := fmt.Sprintf("%s/%s/%s/%s", k.Host, k.RepoPath, k.Package.Arch, k.Package.Filename())
	switch len(k.Package.Checksum) {
	case sha1Size:
		return fmt.Sprintf("%s@sha1:%s", base, hex.EncodeToString(k.Package.Checksum))
	case sha256Size:
		return fmt.Sprintf("%s@sha256:%s", base, hex.EncodeToString(k.Package.Checksum))
	default:
		return base
	}
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
func StatusDigest(checksum []byte) (name.Digest, error) {
	if len(checksum) == 0 {
		return name.Digest{}, errors.New("empty control checksum")
	}
	checksumHex := hex.EncodeToString(checksum)
	syntheticHash := sha256.Sum256([]byte(checksumHex))
	return name.NewDigest(fmt.Sprintf("apk.cgr.dev/__@sha256:%s", hex.EncodeToString(syntheticHash[:])))
}

// StatusDigest returns the status identity pinned by this key's control
// checksum. Parse guarantees the checksum is present, so this cannot fail
// for parsed keys.
func (k *Key) StatusDigest() (name.Digest, error) {
	return StatusDigest(k.Package.Checksum)
}
