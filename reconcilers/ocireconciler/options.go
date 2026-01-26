/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package ocireconciler

import (
	"context"

	"github.com/google/go-containerregistry/pkg/name"
)

// ReconcilerFunc is the function signature invoked for each digest.
//
// The provided name.Digest includes both the registry/repository context and the
// immutable digest that was enqueued.
type ReconcilerFunc func(ctx context.Context, digest name.Digest) error

// Option configures the Reconciler.
type Option func(*Reconciler)

// WithReconciler installs the reconciliation function invoked for every workqueue key.
func WithReconciler(f ReconcilerFunc) Option {
	return func(r *Reconciler) {
		r.reconcileFunc = f
	}
}

// WithNameOptions provides name.Options passed to name.NewDigest (e.g. name.Insecure).
func WithNameOptions(opts ...name.Option) Option {
	return func(r *Reconciler) {
		r.nameOpts = append(r.nameOpts, opts...)
	}
}
