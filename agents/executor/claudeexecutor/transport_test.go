/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// TestIsTransportDrop_HTTP2StreamReset drives the classifier with the error a
// REAL net/http client reports for a REAL RST_STREAM frame. A hand-built
// http2StreamError would match errors.As against itself and certify the
// reflective match as working while production never matched at all, so the
// server speaks raw HTTP/2 frames and the client is stock net/http.
func TestIsTransportDrop_HTTP2StreamReset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code http2.ErrCode
		want bool
	}{
		{"NO_ERROR is a graceful mid-stream close", http2.ErrCodeNo, true},
		{"INTERNAL_ERROR is the reset Vertex sends mid-thinking", http2.ErrCodeInternal, true},
		{"ENHANCE_YOUR_CALM is not retried blind", http2.ErrCodeEnhanceYourCalm, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := streamResetError(t, tc.code)
			// Guard the guard: if the reflective match stopped working, every
			// arm would read "not a drop" and the negative case would still pass.
			if _, ok := errors.AsType[http2StreamError](err); !ok {
				t.Fatalf("errors.AsType did not match net/http's bundled StreamError in %v (%[1]T); the mirror's field names or types have drifted", err)
			}
			if got := isTransportDrop(err); got != tc.want {
				t.Errorf("isTransportDrop(%v) = %v, want = %v", err, got, tc.want)
			}
			if got := isRetryableClaudeError(fmt.Errorf("stream_message failed after 5 retries: %w", err)); got != tc.want {
				t.Errorf("isRetryableClaudeError(wrapped) = %v, want = %v", got, tc.want)
			}
		})
	}
}

func TestIsTransportDrop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"TCP reset by peer, as net reports it", &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}, true},
		{"body ended early", fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF), true},
		{"client connection lost (closeForLostPing sentinel)", errors.New("http2: client connection lost"), true},
		{"graceful GOAWAY", errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=605, ErrCode=NO_ERROR, debug=""`), true},
		{"graceful GOAWAY with debug data", errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=605, ErrCode=NO_ERROR, debug="shutting down"`), true},
		{"GOAWAY with a real code is not retried blind", errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=605, ErrCode=ENHANCE_YOUR_CALM, debug=""`), false},
		{"peer debug data cannot forge the GOAWAY code", errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=605, ErrCode=ENHANCE_YOUR_CALM, debug="ErrCode=NO_ERROR, debug="`), false},
		{"a read that hit the deadline is the deadline, whatever else it carries", fmt.Errorf("%w: %w", context.DeadlineExceeded, &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}), false},
		{"connection refused is not a drop", &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, false},
		{"the deadline is not a drop", context.DeadlineExceeded, false},
		{"cancellation is not a drop", fmt.Errorf("failed to stream Claude response: %w", context.Canceled), false},
		{"a 400 is not a drop", &anthropic.Error{StatusCode: 400}, false},
		{"plain EOF is not a drop (the SDK never surfaces it as an error)", io.EOF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTransportDrop(tc.err); got != tc.want {
				t.Errorf("isTransportDrop(%v) = %v, want = %v", tc.err, got, tc.want)
			}
			if got := isRetryableClaudeError(tc.err); tc.want && !got {
				t.Errorf("isRetryableClaudeError(%v) = false, want true for a transport drop", tc.err)
			}
		})
	}
}

// streamResetError makes a stock net/http client issue an HTTP/2 request against
// a server that answers with headers, a partial body, and then RST_STREAM
// carrying code, and returns the error the client reports. h2c rather than TLS,
// so no certificate; net/http still uses its bundled HTTP/2 stack, which is the
// type under test. x/net's Framer is used as a frame WRITER only.
func streamResetError(t *testing.T, code http2.ErrCode) error {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serveOneResetStream(conn, code)
	}()

	transport := &http.Transport{Protocols: &http.Protocols{}}
	transport.Protocols.SetUnencryptedHTTP2(true)
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)

	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err == nil {
		// Headers arrived before the reset, so the failure surfaces on the
		// body read instead of from Get.
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	<-served
	if err == nil {
		t.Fatal("client reported no error; the server did not reset the stream")
	}
	return err
}

// serveOneResetStream completes the HTTP/2 handshake, answers the first request
// stream with headers and two body bytes while promising more, then resets that
// stream with code.
func serveOneResetStream(conn net.Conn, code http2.ErrCode) {
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		return
	}
	framer := http2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		return
	}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					return
				}
			}
		case *http2.HeadersFrame:
			var buf bytes.Buffer
			enc := hpack.NewEncoder(&buf)
			// content-length promises a body the reset never delivers, so the
			// client cannot treat the stream as complete.
			_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
			_ = enc.WriteField(hpack.HeaderField{Name: "content-length", Value: "64"})
			if err := framer.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      f.StreamID,
				BlockFragment: buf.Bytes(),
				EndHeaders:    true,
			}); err != nil {
				return
			}
			if err := framer.WriteData(f.StreamID, false, []byte("ab")); err != nil {
				return
			}
			_ = framer.WriteRSTStream(f.StreamID, code)
			return
		}
	}
}
