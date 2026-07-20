/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package memquestions provides an in-memory suspend.QuestionStore for tests
// and single-process demos. It enforces the nonce-binding contract: an Answer
// is only surfaced when its QuestionID matches the currently-pending Question
// for the key, so an answer left over from a superseded pause is ignored.
//
// State does not survive a process restart; it is a stand-in for a real human
// transport (a GitHub issue, a Slack thread, a CLI-backed bucket).
package memquestions
