/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package commentack puts a 👀 reaction on comments that people leave on a
// pull request, so they can see the bot has noticed them.
//
// It covers both kinds of comment: conversation comments and inline review
// comments. Only a comment left within the caller's window, such as the last 24
// hours, gets a 👀. A conversation comment must also come after the pull
// request's head commit, and an inline comment in a resolved thread gets none.
// Pending decides which comments still need one. Ack reads a pull request's
// comments and its head commit date and reacts to each pending comment.
package commentack
