/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package commentaudience tells whether a pull request comment is meant for
// the bot. A comment that mentions someone other than the bot is a talk
// between people, so the bot should leave it alone.
package commentaudience
