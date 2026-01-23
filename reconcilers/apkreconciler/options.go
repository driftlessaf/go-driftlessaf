/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package apkreconciler

import (
	"context"

	"chainguard.dev/driftlessaf/reconcilers/apkreconciler/apkurl"
)

// ReconcilerFunc is the function signature invoked for each APK key.
//
// The provided Key contains parsed components from the apkurl CloudEvents
// extension. Use key.URL() to get the full HTTPS URL for fetching the APK,
// or key.FetchablePackage() for use with apko's APK client.
type ReconcilerFunc func(ctx context.Context, key *apkurl.Key) error

// Option configures the Reconciler.
type Option func(*Reconciler)

// WithReconciler installs the reconciliation function invoked for every workqueue key.
func WithReconciler(f ReconcilerFunc) Option {
	return func(r *Reconciler) {
		r.reconcileFunc = f
	}
}
