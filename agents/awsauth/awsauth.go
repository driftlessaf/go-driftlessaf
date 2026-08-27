/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package awsauth

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/sethvargo/go-envconfig"
)

const (
	// EnvRegion names the required AWS region environment variable.
	EnvRegion = "AWS_REGION"
	// EnvProfile names the AWS IAM Identity Center (SSO) profile environment variable.
	EnvProfile = "AWS_PROFILE"
	// EnvRoleARN names the role assumed when using web identity.
	EnvRoleARN = "AWS_ROLE_ARN"
	// EnvWebIdentityTokenFile names the file containing the web-identity token.
	EnvWebIdentityTokenFile = "AWS_WEB_IDENTITY_TOKEN_FILE" //nolint:gosec // G101: environment variable name, not a credential

	envAccessKeyID     = "AWS_ACCESS_KEY_ID"        //nolint:gosec // G101: environment variable name, not a credential
	envSecretAccessKey = "AWS_SECRET_ACCESS_KEY"    //nolint:gosec // G101: environment variable name, not a credential
	envSessionToken    = "AWS_SESSION_TOKEN"        //nolint:gosec // G101: environment variable name, not a credential
	envSecurityToken   = "AWS_SECURITY_TOKEN"       //nolint:gosec // G101: environment variable name, not a credential
	envBearerToken     = "AWS_BEARER_TOKEN_BEDROCK" //nolint:gosec // G101: environment variable name, not a credential
	envAnthropicAPIKey = "ANTHROPIC_AWS_API_KEY"    //nolint:gosec // G101: environment variable name, not a credential
)

// Config identifies an allowed AWS credential configuration.
//
// Profile is set for local AWS IAM Identity Center (SSO) authentication. It is
// empty for web-identity authentication, where the AWS SDK reads EnvRoleARN and
// EnvWebIdentityTokenFile from the environment.
type Config struct {
	Region  string
	Profile string
}

type environment struct {
	Region               string `env:"AWS_REGION"`
	Profile              string `env:"AWS_PROFILE"`
	RoleARN              string `env:"AWS_ROLE_ARN"`
	WebIdentityTokenFile string `env:"AWS_WEB_IDENTITY_TOKEN_FILE"`
	AccessKeyID          string `env:"AWS_ACCESS_KEY_ID"`
	SecretAccessKey      string `env:"AWS_SECRET_ACCESS_KEY"`
	SessionToken         string `env:"AWS_SESSION_TOKEN"`
	SecurityToken        string `env:"AWS_SECURITY_TOKEN"`
	BearerToken          string `env:"AWS_BEARER_TOKEN_BEDROCK"`
	AnthropicAPIKey      string `env:"ANTHROPIC_AWS_API_KEY"`
}

// ConfigFromEnv reads and validates an AWS SSO or web-identity configuration.
// Static AWS credentials and Bedrock API keys are rejected.
func ConfigFromEnv(ctx context.Context) (Config, error) {
	var env environment
	if err := envconfig.Process(ctx, &env); err != nil {
		return Config{}, fmt.Errorf("reading AWS auth environment: %w", err)
	}
	if env.Region == "" {
		return Config{}, fmt.Errorf("AWS authentication requires %s", EnvRegion)
	}
	if name := staticCredentialEnv(env); name != "" {
		return Config{}, fmt.Errorf("AWS authentication does not permit static credentials from %s", name)
	}
	if name := apiKeyEnv(env); name != "" {
		return Config{}, fmt.Errorf("AWS authentication does not permit Bedrock API-key authentication from %s", name)
	}

	hasProfile := env.Profile != ""
	hasRole := env.RoleARN != ""
	hasTokenFile := env.WebIdentityTokenFile != ""
	if hasProfile && (hasRole || hasTokenFile) {
		return Config{}, fmt.Errorf("%s cannot be combined with %s or %s", EnvProfile, EnvRoleARN, EnvWebIdentityTokenFile)
	}
	if hasRole != hasTokenFile {
		return Config{}, fmt.Errorf("%s and %s must be set together", EnvRoleARN, EnvWebIdentityTokenFile)
	}
	if !hasProfile && !hasRole {
		return Config{}, fmt.Errorf("AWS authentication requires %s for AWS SSO or %s and %s for web identity", EnvProfile, EnvRoleARN, EnvWebIdentityTokenFile)
	}
	return Config{Region: env.Region, Profile: env.Profile}, nil
}

// ValidateCredentials verifies that the AWS SDK credential chain selected by
// cfg is backed by AWS IAM Identity Center (SSO) or web identity.
func (cfg Config) ValidateCredentials(ctx context.Context) error {
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.Profile != "" {
		loadOptions = append(loadOptions, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return fmt.Errorf("loading AWS credential configuration: %w", err)
	}
	provider, ok := awsCfg.Credentials.(aws.CredentialProviderSource)
	if !ok {
		return fmt.Errorf("credential provider %T does not expose its AWS source", awsCfg.Credentials)
	}
	sources := provider.ProviderSources()
	if cfg.Profile != "" {
		if !hasSSOCredentialSource(sources) {
			return fmt.Errorf("profile %q must be backed by AWS IAM Identity Center (SSO)", cfg.Profile)
		}
		return nil
	}
	if !slices.Contains(sources, aws.CredentialSourceEnvVarsSTSWebIDToken) {
		return fmt.Errorf("credentials must resolve from %s and %s", EnvRoleARN, EnvWebIdentityTokenFile)
	}
	return nil
}

func staticCredentialEnv(env environment) string {
	switch {
	case env.AccessKeyID != "":
		return envAccessKeyID
	case env.SecretAccessKey != "":
		return envSecretAccessKey
	case env.SessionToken != "":
		return envSessionToken
	case env.SecurityToken != "":
		return envSecurityToken
	default:
		return ""
	}
}

func apiKeyEnv(env environment) string {
	switch {
	case env.BearerToken != "":
		return envBearerToken
	case env.AnthropicAPIKey != "":
		return envAnthropicAPIKey
	default:
		return ""
	}
}

func hasSSOCredentialSource(sources []aws.CredentialSource) bool {
	for _, source := range sources {
		switch source {
		case aws.CredentialSourceProfileSSO,
			aws.CredentialSourceSSO,
			aws.CredentialSourceProfileSSOLegacy,
			aws.CredentialSourceSSOLegacy:
			return true
		}
	}
	return false
}
