/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package systemone calls System One compatible models through their serving
// providers. A request carries one state (text or JSON) and a map of typed
// questions; the response carries one typed, probability-bearing answer per
// question. There are no turns, tools, or generated text, so this package is a
// single-shot client rather than a conversation executor. It shares the
// executor retry policy and GenAI metrics so its requests and token usage
// land in the same telemetry series as the conversational backends.
//
// Three question primitives exist:
//
//   - [Noul] asks a yes/no question and yields the probability of yes.
//   - [Choice] picks one label from a described set and yields the label,
//     a probability per label, and a confidence.
//   - [Score] rates the state on an ordered rubric and yields the weighted
//     position, a probability per level, and a confidence.
//
// Questions in one request are evaluated independently against the same
// state, so one answer never conditions another. Every response is checked
// against the questions as sent: a missing answer, a mismatched kind, an
// undeclared label, or an out-of-range probability is an
// [ErrResponseValidation] rather than a silently accepted value.
//
// To switch serving providers through modelrouter, declare routes with
// [modelrouter.ProtocolSystemOne], resolve the provider/model selection, and
// pass its plan to [WithRoute]. The route supplies the model ID; the provider
// binding supplies its endpoint and authentication. Use [WithExternalAuth]
// when the provider's HTTP transport authenticates the request. Applications
// without a route catalog can use [WithHopper] and [ModelHopper].
//
// Hopper accepts one question per HTTP request and omits some answer fields.
// This client sends multiple questions sequentially, sums their token usage,
// and derives choice confidence, score, legend, and score confidence from the
// returned distribution and the request's rubric. A failed question fails the
// whole Ask call. Hopper's adapter weights are licensed for research and demo
// use only; see https://huggingface.co/HopitAI/hopper.
//
// The client never logs the API key or request bodies. Callers own the
// content they send: package bytes, user text, and other untrusted input are
// forwarded to a third-party API verbatim.
package systemone
