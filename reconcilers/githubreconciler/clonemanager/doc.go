/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package clonemanager provides pooled git clones tailored for reconciler style
// workloads. A Manager is configured with the GitHub token source and commit
// identity for an automation, and exposes Lease handles that:
//   - Hydrate a repository and branch reference into an isolated working tree.
//   - Report metadata about the prepared commit and whether the target path exists.
//   - Offer MakeAndPushChanges, a high-level helper that accepts a callback to
//     apply updates, create a signed commit, and force-push a branch.
//
// Callers typically acquire a lease per reconciliation, operate on the prepared
// working tree through the provided callback, and finally Return the lease to
// reset and reuse the clone. Clones are recycled behind the scenes, avoiding the
// overhead of re-cloning for every reconciliation loop.
package clonemanager
