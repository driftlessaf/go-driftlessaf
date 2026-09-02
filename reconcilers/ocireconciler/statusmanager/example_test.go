/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v3/pkg/cosign"
)

// ExampleStatus demonstrates constructing a Status value with custom details.
func ExampleStatus() {
	type MyDetails struct {
		Result string
	}

	s := statusmanager.Status[MyDetails]{
		ObservedGeneration: "sha256:abc123",
		Details:            MyDetails{Result: "success"},
	}
	fmt.Printf("generation=%s result=%s\n", s.ObservedGeneration, s.Details.Result)
	// Output: generation=sha256:abc123 result=success
}

type versionedDetails struct{}

func (versionedDetails) PredicateType() string {
	return "https://status.example.dev/scan/v2"
}

func ExampleWithPredicateType() {
	ctx := context.Background()
	manager, err := statusmanager.NewReadOnly[versionedDetails](ctx,
		statusmanager.WithExpectedIdentity(cosign.Identity{
			Issuer:  "https://accounts.google.com",
			Subject: "producer@example.iam.gserviceaccount.com",
		}),
	)
	if err != nil {
		return
	}
	digest, err := name.NewDigest("example.com/package@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		return
	}
	observed, err := manager.NewSession(digest).ObservedStateWithOptions(ctx,
		statusmanager.WithPredicateType("https://status.example.dev/scan/v1"),
	)
	if err != nil {
		return
	}
	_ = observed
}
