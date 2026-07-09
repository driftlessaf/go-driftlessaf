/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package breaker_test

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"chainguard.dev/driftlessaf/breaker"
)

func ExampleNewTransport() {
	client := &http.Client{
		Timeout:   time.Minute,
		Transport: breaker.NewTransport(nil, breaker.WithFailureThreshold(3)),
	}

	resp, err := client.Get("https://packages.example.dev/os/x86_64/APKINDEX.tar.gz")
	var berr *breaker.Error
	if errors.As(err, &berr) {
		// Transient failure: requeue not before berr.RetryAfter.
		return
	}
	if err == nil {
		resp.Body.Close()
	}
}

func Example() {
	b := breaker.New(
		breaker.WithFailureThreshold(3),
		breaker.WithBaseDelay(time.Second),
		breaker.WithMaxDelay(time.Minute),
	)

	const host = "packages.example.dev"
	for range 3 {
		b.RecordFailure(host)
	}
	ok, _ := b.Allow(host)
	fmt.Println("allowed after three failures:", ok)

	b.RecordSuccess(host)
	ok, _ = b.Allow(host)
	fmt.Println("allowed after a success:", ok)

	// Output:
	// allowed after three failures: false
	// allowed after a success: true
}
