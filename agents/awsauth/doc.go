/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package awsauth provides AWS workload-identity configuration and validation
// for DriftlessAF agents.
//
// Local agents authenticate with an AWS IAM Identity Center (SSO) profile.
// Workloads authenticate with a web-identity token and role ARN. Static AWS
// credentials and Bedrock API keys are rejected. ConfigFromEnv validates the
// selected mode, and Config.ValidateCredentials verifies the source chosen by
// the AWS SDK credential chain. Config.LoadAWSConfig additionally resolves the
// same refreshable provider for a model transport to bind directly. This keeps
// AWS authentication policy independent of any model transport.
package awsauth
