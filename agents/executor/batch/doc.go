/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package batch runs Claude and Gemini requests through Vertex AI batch
// prediction, which bills at about half the online rate in exchange for
// results that arrive within hours rather than seconds. It is a single-shot
// client, not a conversation executor: a job is one provider, one model, and
// a set of independent requests, each answered once.
//
// Nothing in the package waits. [Client.SubmitClaude] and
// [Client.SubmitGemini] stage the input in Cloud Storage and create the job;
// [Client.Status] reports progress; [Client.Results] streams the outcomes
// written so far, and once the job is terminal also reports the keys that
// have none; the caller decides when to check again.
//
// # Submission identity
//
// Vertex job creation carries no request id, so a lost response would
// otherwise submit the rows twice. Every submission names a caller-chosen
// id, used as the job's display name and its storage directory. Before each
// create attempt the client calls [Client.Find], so resubmitting an id, or
// retrying after a lost response, returns the existing job. A job is adopted
// only when its display name, input URI, and output destination all match;
// one that reads the id's input but writes elsewhere fails the submission
// with [ErrInvalidRequest]. A caller that means to run the same rows again
// uses a new id.
//
// # Models
//
// Callers pass bare Vertex model IDs ("claude-opus-5-5@default",
// "gemini-3.8-flash"). The publisher path is added only at the Vertex
// boundary, so the prefixed form never reaches model routing.
//
// # Correlation
//
// Each request carries a key that comes back on its [Result]. Claude lines
// carry a digest of the key as custom_id. Gemini lines have no such field,
// so the digest goes in a request label, and a manifest of canonical request
// digests matches the echoed request when Vertex does not echo the label.
// Keys with no output once the job is terminal yield [ErrMissing].
//
// # Outcomes
//
// Classification matches the online clients and fails closed. A Claude
// stop_reason of "refusal", a Gemini prompt blockReason, or any Gemini
// finishReason other than STOP or MAX_TOKENS (including reasons this
// package does not know) yields [ErrRefused] with the provider's category
// and detail in [Result.Refusal]; the package does not interpret the
// category. A refused result carries no text, matching the online clients,
// which never accept partial content. A max-tokens stop yields
// [ErrTruncated] and keeps the partial text. A finished response
// with no text, and a Gemini response with no candidates, are transient
// per-line errors. Per-line provider failures are returned as [Result.Err],
// and [IsRetryable] tells transient ones apart. The package never resubmits
// on its own.
//
// # Telemetry
//
// With [WithMetrics], each yielded result records its tokens and outcome in
// the same genai.* series as the online executors, under
// gen_ai.operation.name "batch_predict" and gen_ai.provider.name
// "gcp.vertex_ai".
//
// Request content is staged in the bucket in plain JSONL; callers own its
// retention and access.
package batch
