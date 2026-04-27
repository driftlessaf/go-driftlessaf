/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

// Status is the materializer's progress state on a Linear issue. It is a
// distinct string type so callers cannot assign arbitrary strings (typos
// like "actve" become compile-time errors).
type Status string

// Status values persisted on the Linear issue's state attachment.
const (
	StatusActive         Status = "active"
	StatusGreen          Status = "green"
	StatusFailed         Status = "failed"
	StatusWaitingForRepo Status = "waiting_for_repo"
)

// MaterializerState tracks the materializer's progress on a Linear issue.
// This is persisted as a file attachment on the issue via the StateManager
// with the "materializer" prefix.
type MaterializerState struct {
	PRURL  string `json:"pr_url,omitempty"`
	Status Status `json:"status,omitempty"`
}
