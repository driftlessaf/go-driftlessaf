/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package calltiming measures the network timing of one model call from
// net/http/httptrace hooks: TCP connect, TLS handshake, connection reuse, and
// the time from the request being written to the first response byte and to
// the first streamed token. The executors attach a Timer to the context they
// pass to their SDK and record the result on the trace turn.
//
// A Timer's hooks read timestamps, the dial's network and address (to pair a
// dial's start with its end), the connection-reuse flag, and whether each
// phase failed. They ignore the TLS state they are passed and never see
// headers, bodies, or the connection, so a signing boundary that hides caller
// trace hooks (bedrockruntime) can re-attach a Timer's hooks to the signed
// request without exposing its credentials.
package calltiming
