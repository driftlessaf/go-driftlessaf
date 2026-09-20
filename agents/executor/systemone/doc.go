/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package systemone calls TypeSafe AI's System One API, whose flagship model
// is Jev. A request carries one state (text or JSON) and a map of typed
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
// The client never logs the API key or request bodies. Callers own the
// content they send: package bytes, user text, and other untrusted input are
// forwarded to a third-party API verbatim.
package systemone
