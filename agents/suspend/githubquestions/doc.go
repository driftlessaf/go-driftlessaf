/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package githubquestions implements suspend.QuestionStore over GitHub PR
// (or issue) comments, so the human half of an ask-human pause happens where
// humans already are: the question appears as a comment on the conversation
// the paused run belongs to, and a collaborator answers by replying
//
//	/answer <text>
//
// # How state is carried
//
// The conversation itself is the store. Ask posts a comment whose visible
// text is the question and whose hidden HTML marker carries the full
// suspend.Question as base64 JSON — base64 so the marker survives any prompt
// content (a literal "-->" would otherwise terminate the HTML comment). The
// newest marker-bearing comment is the pending question; Consume rewrites
// that comment (marker retired, "answered" note appended) so it stops being
// pending. Nothing is stored outside GitHub, so a different process — or a
// different machine — resumes the lifecycle from the conversation alone.
//
// # Authorization
//
// Only replies whose author association is OWNER, MEMBER, or COLLABORATOR
// are accepted, and app/bot authors are ignored. Answering steers a paused
// agent, so the ability to answer is deliberately tied to write-side
// repository membership rather than to anyone who can comment.
//
// The question channel is gated the other way around: pending-question
// markers are only honored in app/bot-authored comments, since questions are
// posted by the reconciler's app identity. A marker forged by a human
// commenter — which would otherwise supersede the real question and deny the
// resume — is ignored. Apps installed on the repository remain trusted; they
// are admin-granted.
//
// # Race semantics
//
// GitHub comment edits are not compare-and-swap. The lifecycle's exactly-once
// guarantee comes from the checkpoint Store's claim (a CAS delete on the
// parked envelope), not from this transport: a racing Consume or a
// superseding Ask degrades to an extra comment edit, never a double resume.
// Nonce binding still applies — an answer is only surfaced for the question
// whose marker is currently pending, so a reply to a stale, superseded
// question is never injected.
package githubquestions
