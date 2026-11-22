/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
)

// isTransportDrop reports whether err is the transport dropping a response
// stream mid-flight with the request itself intact: the peer reset the HTTP/2
// stream with NO_ERROR or INTERNAL_ERROR, the TCP connection was reset, the
// body ended early, or the client connection was lost or told GOAWAY. The
// turn's message is appended only after the stream completes, so re-sending
// the same request duplicates nothing. Context cancellation and API errors
// are not transport drops.
//
// Every shape here was observed in production and re-running the turn
// succeeded: 35 drops in 3 days on the range-enrichment Jobs alone
// (2026-09-22..24), 26 of them the first two shapes.
func isTransportDrop(err error) bool {
	if err == nil {
		return false
	}
	// The deadline is the truth even when the read that hit it also carries
	// a transport cause.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if h2, ok := errors.AsType[http2StreamError](err); ok {
		return h2.Code == http2ErrCodeNo || h2.Code == http2ErrCodeInternal
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// An API error carries a verdict, so it is never a transport drop; it is
	// also the one error whose Error() needs its request populated.
	if _, ok := errors.AsType[*anthropic.Error](err); ok {
		return false
	}
	// net/http exposes these two as text only: closeForLostPing wraps an
	// errors.New sentinel, and its GoAwayError struct has no As method.
	msg := err.Error()
	if strings.Contains(msg, "http2: client connection lost") {
		return true
	}
	return isGracefulGoAway(msg)
}

// isGracefulGoAway matches GoAwayError's fixed rendering, "http2: server sent
// GOAWAY and closed the connection; LastStreamID=%v, ErrCode=%v, debug=%q", in
// field order: the peer's debug data comes last, so it cannot forge the code.
func isGracefulGoAway(msg string) bool {
	_, rest, ok := strings.Cut(msg, "http2: server sent GOAWAY and closed the connection; LastStreamID=")
	if !ok {
		return false
	}
	return strings.HasPrefix(strings.TrimLeft(rest, "0123456789"), ", ErrCode=NO_ERROR, debug=")
}

// net/http's HTTP/2 stack is a bundled internal copy of golang.org/x/net/http2,
// so its StreamError is not assignable to the exported x/net type. The bundled
// type implements errors.As reflectively against any struct whose field names
// and types match (its errors.go), which is what makes the mirror below match.
// The field names and their order are the match key.
type http2ErrCode uint32

const (
	http2ErrCodeNo       http2ErrCode = 0x0 // RST_STREAM NO_ERROR
	http2ErrCodeInternal http2ErrCode = 0x2 // RST_STREAM INTERNAL_ERROR
)

type http2StreamError struct {
	StreamID uint32
	Code     http2ErrCode
	Cause    error
}

func (e http2StreamError) Error() string {
	return fmt.Sprintf("http2 stream %d reset with code %d", e.StreamID, e.Code)
}
