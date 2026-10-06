/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package model is a registry of provider-side model capabilities keyed by
// model id.
//
// Resolve maps a model id to an Info describing which request parameters the
// provider accepts for it: the backend the id routes to, the effort levels
// the provider takes natively, whether the sampling parameters (temperature,
// top_p, top_k) are accepted, whether the Claude extended-thinking budget
// parameter is accepted, and which generation of Gemini thinking knob
// applies. Jev ids ("jev-") and Hopper resolve to a backend with
// no parameter surface at all, since those models take typed questions rather
// than a conversation.
//
// The registry encodes generation rules plus exception prefixes rather than
// an exhaustive id list: capabilities derive from the id's backend and
// version, and small prefix tables list only the models verified to lack a
// parameter. An id matching no exception prefix therefore resolves to the
// newest capability surface for its backend — a deliberate bias, so a
// freshly released model works without a registry change and the exception
// tables grow only when a model is verified to reject a parameter.
//
// Info.ContextWindow is the exception to that bias. It is the serving context
// window in tokens, input and output together, and an executor that enforces
// an input budget trusts it. A wrong window either rejects requests that
// fit or lets requests through that the provider refuses. It therefore comes
// from a table of exact base ids (the id before "@"), not prefixes, and lists
// only windows verified against the provider. Every other id reports 0,
// which means unknown, and a caller that needs a window must supply it.
// Add an id to the table only after its window is verified.
package model
