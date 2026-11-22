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
	"net"
	"os"
	"syscall"
	"testing"
)

// The HTTP/2 stream-reset shapes need a real net/http client to prove the
// mirror matches; claudeexecutor's transport tests drive one.
func TestIsTransportDrop(t *testing.T) {
	t.Parallel()
	errVerdict := errors.New("provider verdict")
	hasVerdict := func(err error) bool { return errors.Is(err, errVerdict) }
	reset := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}

	for _, tc := range []struct {
		name       string
		err        error
		hasVerdict func(error) bool
		want       bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "TCP reset by peer", err: reset, want: true},
		{name: "body ended early", err: fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF), want: true},
		{name: "client connection lost", err: errors.New("http2: client connection lost"), want: true},
		{name: "graceful GOAWAY", err: errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=605, ErrCode=NO_ERROR, debug=""`), want: true},
		{name: "GOAWAY with a real code", err: errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=605, ErrCode=ENHANCE_YOUR_CALM, debug=""`), want: false},
		{name: "peer debug data cannot forge the GOAWAY code", err: errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=605, ErrCode=ENHANCE_YOUR_CALM, debug="ErrCode=NO_ERROR, debug="`), want: false},
		{name: "deadline wins over a reset", err: fmt.Errorf("%w: %w", context.DeadlineExceeded, reset), want: false},
		{name: "cancellation", err: fmt.Errorf("sending: %w", context.Canceled), want: false},
		{name: "connection refused", err: &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, want: false},
		{name: "plain EOF", err: io.EOF, want: false},
		{name: "verdict quoting a lost connection", err: fmt.Errorf("%w: http2: client connection lost", errVerdict), hasVerdict: hasVerdict, want: false},
		{name: "lost connection without a verdict check", err: fmt.Errorf("%w: http2: client connection lost", errVerdict), want: true},
		{name: "verdict does not hide a reset under it", err: fmt.Errorf("%w: %w", errVerdict, reset), hasVerdict: hasVerdict, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTransportDrop(tc.err, tc.hasVerdict); got != tc.want {
				t.Errorf("IsTransportDrop(%v): got = %v, want = %v", tc.err, got, tc.want)
			}
		})
	}
}
