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

// ExampleConfig_LoadAWSConfig demonstrates resolving the validated,
// refreshable credential provider that a transport binds directly.
func ExampleConfig_LoadAWSConfig() {
	cfg := awsauth.Config{Region: "us-east-1", Profile: "engineering-sso"}
	awsConfig, err := cfg.LoadAWSConfig(context.Background())
	if err != nil {
		return
	}
	fmt.Printf("AWS region: %s\n", awsConfig.Region)
}
