/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package executor holds the contract shared by the agent executors
// (claudeexecutor, googleexecutor, openaiexecutor): the conditions every
// executor reports the same way, so a caller can handle them without knowing
// which provider ran the agent.
package executor
