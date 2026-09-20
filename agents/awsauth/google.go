/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package awsauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func (cfg Config) validateGoogle(env environment) error {
	if err := env.validateSecrets(); err != nil {
		return err
	}
	if cfg.Google.RoleARN == "" || cfg.Google.Audience == "" {
		return errors.New("workload identity for Google requires a role ARN and audience")
	}
	if cfg.Profile != "" || env.Profile != "" || env.WebIdentityTokenFile != "" {
		return errors.New("workload identity for Google cannot be combined with an AWS profile or web-identity token file")
	}
	if env.RoleARN != "" && env.RoleARN != cfg.Google.RoleARN {
		return errors.New("workload identity for Google role conflicts with AWS_ROLE_ARN")
	}
	if env.GoogleAudience != "" && env.GoogleAudience != cfg.Google.Audience {
		return errors.New("workload identity for Google audience conflicts with AWS_GOOGLE_AUDIENCE")
	}
	return nil
}

func (cfg Config) loadGoogleConfig(ctx context.Context) (aws.Config, error) {
	if err := cfg.validateGoogle(readEnvironment()); err != nil {
		return aws.Config{}, err
	}
	// Supply an explicit anonymous provider: STS web identity requires no AWS
	// signing credentials, and must never discover another credential source.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}),
	)
	if err != nil {
		return aws.Config{}, errors.New("loading Google workload AWS configuration failed")
	}
	awsCfg.Credentials = aws.NewCredentialsCache(&googleProvider{
		config: cfg.Google, client: sts.NewFromConfig(awsCfg),
	}, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = time.Minute })
	return awsCfg, nil
}

type googleProvider struct {
	config GoogleConfig
	client *sts.Client
}

var _ aws.CredentialsProvider = (*googleProvider)(nil)

// Retrieve performs one identity exchange. The AWS credentials cache calls it
// on demand when empty or within one minute of the STS expiration. Bedrock's
// signer retrieves from that cache for every request, so an existing agent
// refreshes without reconstruction or a background refresh timer.
func (p *googleProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	// Obtain a fresh identity token for each STS refresh. Neither the constructor
	// context nor its cancellation is retained for future credential refreshes.
	token, err := googleMetadata.GetWithContext(ctx, "instance/service-accounts/default/identity?audience="+url.QueryEscape(p.config.Audience)+"&format=full")
	if err != nil || token == "" {
		if ctx.Err() != nil {
			return aws.Credentials{}, ctx.Err()
		}
		// Metadata errors can contain response bodies; never surface identity tokens.
		return aws.Credentials{}, errors.New("retrieving Google workload identity token failed")
	}
	provider := stscreds.NewWebIdentityRoleProvider(p.client, p.config.RoleARN, identityToken(token), func(o *stscreds.WebIdentityRoleOptions) {
		o.Duration = time.Hour
	})
	credentials, err := provider.Retrieve(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return aws.Credentials{}, ctx.Err()
		}
		// STS response messages can echo submitted identity tokens. Preserve a stable
		// operation label without propagating response bodies into application logs.
		return aws.Credentials{}, errors.New("exchanging Google workload identity with AWS STS failed")
	}
	return credentials, nil
}

// identityToken holds the Google token for one STS exchange. googleProvider
// obtains a fresh token and constructs a new wrapper on every cache refresh.
type identityToken string

var _ stscreds.IdentityTokenRetriever = identityToken("")

func (t identityToken) GetIdentityToken() ([]byte, error) { return []byte(t), nil }

// The metadata response is sensitive and small. Bound it before the SDK reads
// the body, disable redirects, and do not send metadata requests through proxies.
var googleMetadata = metadata.NewClient(&http.Client{
	Transport:     metadataTransport{base: &http.Transport{}},
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
})

type metadataTransport struct{ base http.RoundTripper }

var _ http.RoundTripper = (*metadataTransport)(nil)

func (t metadataTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	response.Body = http.MaxBytesReader(nil, response.Body, 64<<10)
	return response, nil
}
