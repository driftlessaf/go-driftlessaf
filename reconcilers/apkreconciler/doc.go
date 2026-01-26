/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package apkreconciler provides a workqueue-based reconciliation framework for APK packages.
//
// This package enables building reconcilers that process APK keys from a workqueue,
// similar to how ocireconciler handles OCI image digests. APK reconcilers receive
// a parsed apkurl.Key and can fetch the package to extract metadata and generate SBOMs.
//
// # Key Format
//
// APK keys are of the form "{host}/{uidp}/{arch}/{package}-{version}.apk" and do
// NOT include the scheme (https://). This matches the format of the apkurl CloudEvents
// extension. Use key.URL() to get the full HTTPS URL for fetching the APK.
//
// Example key: "apk.cgr.dev/a1b2c3d4.../x86_64/glibc-2.42-r0.apk"
//
// # Basic Usage
//
// Create a reconciler by providing a ReconcilerFunc that processes each APK key:
//
//	r := apkreconciler.New(
//	    apkreconciler.WithReconciler(func(ctx context.Context, key *apkurl.Key) error {
//	        // Fetch the APK using the parsed key
//	        resp, err := http.Get(key.URL().String())
//	        if err != nil {
//	            return fmt.Errorf("fetching APK: %w", err)
//	        }
//	        defer resp.Body.Close()
//
//	        // Access parsed components
//	        fmt.Printf("Package: %s, Version: %s, Arch: %s\n",
//	            key.Package.Name, key.Package.Version, key.Package.Arch)
//
//	        // Or use apko's FetchablePackage interface
//	        fetchable := key.FetchablePackage()
//	        return nil
//	    }),
//	)
//
// # Workqueue Integration
//
// The Reconciler implements the WorkqueueServiceServer interface, making it easy
// to deploy as a regional-go-reconciler:
//
//	workqueue.RegisterWorkqueueServiceServer(grpcServer, reconciler)
//
// Keys enqueued to the workqueue come from the apkurl CloudEvents extension on
// APK push events.
//
// # Status Management
//
// APK status is stored as OCI attestations using a synthetic digest reference
// of the form "apk.cgr.dev/{uidp}@sha256:{datahash}". This allows reusing the
// ocireconciler/statusmanager infrastructure for APK reconciliation state.
//
// # Thread Safety
//
// The Reconciler is safe for concurrent use. Multiple Process calls can execute
// simultaneously, each with its own context and APK key.
package apkreconciler
