/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package awsauth_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/awsauth"
)

// Example demonstrates resolving an allowed AWS authentication mode and
// verifying the credential source selected by the AWS SDK.
func Example() {
	cfg, err := awsauth.ConfigFromEnv(context.Background())
	if err != nil {
		return
	}
	if err := cfg.ValidateCredentials(context.Background()); err != nil {
		return
	}
	fmt.Printf("AWS region: %s\n", cfg.Region)
}
