/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsTransientRekorError(t *testing.T) {
	for _, err := range []error{
		errors.New("adding rekor v2 entry: unexpected response: 499 context canceled"),
		errors.New("adding rekor v2 entry: unexpected response: 429 rate limited"),
		errors.New("adding rekor v2 entry: unexpected response: 503 no healthy upstream"),
		fmt.Errorf("adding rekor v2 entry: getting response: %w", &url.Error{Err: errors.New("connection reset")}),
		fmt.Errorf("adding rekor v2 entry: getting response: %w", context.DeadlineExceeded),
		fmt.Errorf("adding rekor v2 entry: reading response: %w", io.ErrUnexpectedEOF),
	} {
		require.True(t, isTransientRekorError(err), err)
	}

	for _, err := range []error{
		nil,
		errors.New("adding rekor v2 entry: unexpected response: 400 invalid entry"),
		errors.New("adding rekor v2 entry: unexpected response: unavailable"),
		errors.New("pushing manifest: unexpected response: 503 no healthy upstream"),
	} {
		require.False(t, isTransientRekorError(err), err)
	}
}
