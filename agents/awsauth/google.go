/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package awsauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	httpmetrics "github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
)

var serviceAccountEmail = regexp.MustCompile(`^[a-z0-9-]+@[a-z0-9.-]+\.iam\.gserviceaccount\.com$`)

// Validate reports whether g is a usable Google workload identity: RoleARN
// and Audience are set, and ServiceAccount, when set, is a
// *.iam.gserviceaccount.com email. It loads no credentials, so a caller can
// reject a malformed configuration at startup. The zero GoogleConfig, which
// Config treats as "Google not selected", fails.
func (g GoogleConfig) Validate() error {
	if g.RoleARN == "" || g.Audience == "" {
		return errors.New("workload identity for Google requires a role ARN and audience")
	}
	if g.ServiceAccount != "" && !serviceAccountEmail.MatchString(g.ServiceAccount) {
		return errors.New("workload identity for Google service account must be a *.iam.gserviceaccount.com email")
	}
	return nil
}

func (cfg Config) validateGoogle(env environment) error {
	if err := env.validateSecrets(); err != nil {
		return err
	}
	if err := cfg.Google.Validate(); err != nil {
		return err
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
	provider := &googleProvider{config: cfg.Google, client: sts.NewFromConfig(awsCfg)}
	if cfg.Google.ServiceAccount != "" {
		// httpmetrics may wrap http.DefaultTransport; clone the inner transport when it can.
		inner, ok := httpmetrics.ExtractInnerTransport(http.DefaultTransport).(*http.Transport)
		if ok {
			inner = inner.Clone()
		} else {
			inner = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
		// Only the metadata server's credentials authorize the call, never local
		// ADC such as key files or gcloud user credentials.
		client := &http.Client{
			Transport: boundedTransport{base: &oauth2.Transport{
				Source: oauth2.ReuseTokenSourceWithExpiry(nil, metadataAccessToken{}, time.Minute),
				Base:   inner,
			}},
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		// The SDK's debug logs include response bodies, and those carry tokens.
		quiet := option.WithLogger(slog.New(slog.DiscardHandler))
		service, err := iamcredentials.NewService(ctx, append([]option.ClientOption{quiet, option.WithHTTPClient(client)}, iamCredentialsOptions...)...)
		if err != nil {
			return aws.Config{}, errors.New("creating IAM Credentials client failed")
		}
		provider.idTokens = service.Projects.ServiceAccounts
	}
	awsCfg.Credentials = aws.NewCredentialsCache(provider, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = time.Minute })
	return awsCfg, nil
}

// iamCredentialsOptions lets tests point the IAM Credentials client at a fake.
var iamCredentialsOptions []option.ClientOption

type googleProvider struct {
	config GoogleConfig
	client *sts.Client
	// idTokens mints tokens as config.ServiceAccount; nil uses the metadata server.
	idTokens *iamcredentials.ProjectsServiceAccountsService
}

var _ aws.CredentialsProvider = (*googleProvider)(nil)

// Retrieve performs one identity exchange. The AWS credentials cache calls it
// on demand when empty or within one minute of the STS expiration. Bedrock's
// signer retrieves from that cache for every request, so an existing agent
// refreshes without reconstruction or a background refresh timer.
func (p *googleProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	// Obtain a fresh identity token for each STS refresh. No refresh inherits the
	// constructor context's cancellation.
	var token string
	var err error
	if p.idTokens == nil {
		token, err = googleMetadata.GetWithContext(ctx, "instance/service-accounts/default/identity?audience="+url.QueryEscape(p.config.Audience)+"&format=full")
	} else {
		// includeEmail matches the email claims of the metadata server's format=full token.
		var response *iamcredentials.GenerateIdTokenResponse
		response, err = p.idTokens.GenerateIdToken("projects/-/serviceAccounts/"+p.config.ServiceAccount, &iamcredentials.GenerateIdTokenRequest{
			Audience: p.config.Audience, IncludeEmail: true,
		}).Context(ctx).Do()
		if err == nil {
			token = response.Token
		}
	}
	if err != nil || token == "" {
		if ctx.Err() != nil {
			return aws.Credentials{}, ctx.Err()
		}
		// Error bodies can carry token material; keep only the HTTP status.
		if apiErr, ok := errors.AsType[*googleapi.Error](err); ok {
			return aws.Credentials{}, fmt.Errorf("generating Google ID token as service account failed with HTTP %d", apiErr.Code)
		}
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

// metadataAccessToken fetches the metadata server's access token. Refreshes
// outlive the constructor, so they take no caller context.
type metadataAccessToken struct{}

var _ oauth2.TokenSource = (*metadataAccessToken)(nil)

func (metadataAccessToken) Token() (*oauth2.Token, error) {
	// The metadata client retries, so bound the whole fetch, not each attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, err := googleMetadata.GetWithContext(ctx, "instance/service-accounts/default/token?scopes="+url.QueryEscape(iamcredentials.CloudPlatformScope))
	if err != nil {
		return nil, errors.New("retrieving metadata access token failed")
	}
	var token oauth2.Token
	if err := json.Unmarshal([]byte(body), &token); err != nil || token.AccessToken == "" || token.ExpiresIn <= 0 {
		return nil, errors.New("metadata access token response is invalid")
	}
	token.Expiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return &token, nil
}

// identityToken holds the Google token for one STS exchange. googleProvider
// obtains a fresh token and constructs a new wrapper on every cache refresh.
type identityToken string

var _ stscreds.IdentityTokenRetriever = identityToken("")

func (t identityToken) GetIdentityToken() ([]byte, error) { return []byte(t), nil }

// The metadata response is sensitive and small. Bound it before the SDK reads
// the body, disable redirects, and do not send metadata requests through proxies.
var googleMetadata = metadata.NewClient(&http.Client{
	Transport:     boundedTransport{base: &http.Transport{}},
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
})

// boundedTransport caps identity token response bodies at 64 KiB.
type boundedTransport struct{ base http.RoundTripper }

var _ http.RoundTripper = (*boundedTransport)(nil)

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	response.Body = http.MaxBytesReader(nil, response.Body, 64<<10)
	return response, nil
}
