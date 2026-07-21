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
// applies.
//
// The registry encodes generation rules plus exception prefixes rather than
// an exhaustive id list: capabilities derive from the id's backend and
// version, and small prefix tables list only the models verified to lack a
// parameter. An id matching no exception prefix therefore resolves to the
// newest capability surface for its backend — a deliberate bias, so a
// freshly released model works without a registry change and the exception
// tables grow only when a model is verified to reject a parameter.
package model
