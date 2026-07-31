/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package apkurl_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/apkreconciler/apkurl"
)

// ExampleParse demonstrates parsing an APK URL key into its components.
// Keys pin the APK's control checksum, so the status identity is available
// without fetching the APK.
func ExampleParse() {
	key, err := apkurl.Parse("packages.wolfi.dev/os/x86_64/glibc-2.42-r0.apk@sha1:da39a3ee5e6b4b0d3255bfef95601890afd80709")
	if err != nil {
		panic(err)
	}
	fmt.Printf("Host: %s\n", key.Host)
	fmt.Printf("Package: %s\n", key.Package.Name)
	fmt.Printf("Version: %s\n", key.Package.Version)
	fmt.Printf("Arch: %s\n", key.Package.Arch)
	fmt.Printf("Checksum: %x\n", key.Package.Checksum)
	// Output:
	// Host: packages.wolfi.dev
	// Package: glibc
	// Version: 2.42-r0
	// Arch: x86_64
	// Checksum: da39a3ee5e6b4b0d3255bfef95601890afd80709
}
