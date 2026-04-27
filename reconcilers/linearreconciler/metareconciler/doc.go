/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package metareconciler provides a generic reconciler that bridges Linear
// issues to GitHub PRs via an AI agent. It mirrors the
// githubreconciler/metareconciler but uses Linear as the issue source.
//
// The package is parameterized by request type, response type, and callbacks
// type so different agents can plug in their specific logic while reusing
// the common reconciliation infrastructure.
//
// # Reconciliation flow
//
//  1. Resolve repo target from the Linear issue (upstream bot state or fallback)
//  2. Open a change session against the GitHub PR
//  3. Inspect PR state (skip / closed / findings / pending)
//  4. Acquire clone lease
//  5. Run the agent
//  6. Push changes and update Linear status
//
// # Feedback loop
//
// In addition to handling Linear-keyed events, the reconciler can be wrapped
// with WrapWithPRHandler so the same workqueue accepts GitHub PR URLs.
// PR events extract the originating Linear issue UUID from data embedded in
// the PR body and re-queue the issue, enabling CI feedback to drive
// further iterations.
package metareconciler
