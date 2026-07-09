/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package transient_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"chainguard.dev/driftlessaf/reconcilers/transient"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// ExampleMark marks and detects transient failures; marks survive wrapping.
func ExampleMark() {
	err := fmt.Errorf("writing attestation bundle: %w", transient.Mark(errors.New("upstream 503")))
	fmt.Println(transient.Is(err))
	fmt.Println(transient.Is(errors.New("hard failure")))
	// Output:
	// true
	// false
}

// ExampleRetry absorbs a short upstream blip in-process, using Is itself as
// the retryable predicate.
func ExampleRetry() {
	calls := 0
	err := transient.Retry(context.Background(), "pushing attestation", transient.Is, func(context.Context) error {
		calls++
		if calls == 1 {
			return &transport.Error{StatusCode: http.StatusServiceUnavailable}
		}
		return nil
	})
	fmt.Println(err, calls)
	// Output: <nil> 2
}

// ExampleIs also reports temporary registry errors as transient.
func ExampleIs() {
	err := fmt.Errorf("pulling layer: %w", &transport.Error{StatusCode: http.StatusServiceUnavailable})
	fmt.Println(transient.Is(err))
	// Output: true
}
