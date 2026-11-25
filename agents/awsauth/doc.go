/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package awsauth provides AWS workload-identity configuration and validation
// for DriftlessAF agents.
//
// Local agents authenticate with an AWS IAM Identity Center (SSO) profile.
// Google Cloud workloads can explicitly exchange metadata identity tokens for
// AWS credentials, refreshed in process. With GoogleConfig.ServiceAccount set,
// the token is instead minted as that service account through IAM
// Credentials, authorized by the metadata server's credentials, which need
// iam.serviceAccounts.getOpenIdToken on it. Local ADC is never used. Other
// workloads authenticate with a web-identity token file and role ARN. Static
// AWS credentials and Bedrock API keys are rejected. ConfigFromEnv validates
// the selected mode, and Config.ValidateCredentials verifies the source chosen
// by the AWS SDK credential chain. Config.LoadAWSConfig additionally resolves
// the same refreshable provider for a model transport to bind directly. This
// keeps AWS authentication policy independent of any model transport.
package awsauth
