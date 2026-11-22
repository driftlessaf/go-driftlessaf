/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
)

// IsTransportDrop reports whether err is the transport dropping a response
// mid-flight with the request itself intact: the peer reset the HTTP/2
// stream with NO_ERROR or INTERNAL_ERROR, the TCP connection was reset, the
// body ended early, or the client connection was lost or told GOAWAY.
// Context cancellation and deadlines are not transport drops.
//
// Each of these shapes was observed in production, and re-sending the
// request succeeded. Re-sending is safe only for a caller that records a
// response once it is complete, so a dropped attempt leaves nothing behind.
//
// hasVerdict reports whether err carries the provider API's answer to the
// request. Such an error is never a transport drop, and its message is not
// read: the provider wrote it, and some SDK error types need request state to
// render it. A nil hasVerdict treats no error as a verdict.
func IsTransportDrop(err error, hasVerdict func(error) bool) bool {
	if err == nil {
		return false
	}
	// The deadline is the truth even when the read that hit it also carries
	// a transport cause.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if h2, ok := errors.AsType[HTTP2StreamError](err); ok {
		return h2.Code == http2ErrCodeNo || h2.Code == http2ErrCodeInternal
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if hasVerdict != nil && hasVerdict(err) {
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

// HTTP2StreamError mirrors net/http's bundled HTTP/2 StreamError, so
// errors.As can match the RST_STREAM a peer sent.
type HTTP2StreamError struct {
	StreamID uint32
	Code     http2ErrCode
	Cause    error
}

func (e HTTP2StreamError) Error() string {
	return fmt.Sprintf("http2 stream %d reset with code %d", e.StreamID, e.Code)
}
