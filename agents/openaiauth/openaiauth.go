/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3/auth"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

const (
	// DefaultBaseURL is OpenAI's first-party API, used when Config.BaseURL is
	// empty.
	DefaultBaseURL = "https://api.openai.com/v1/"

	// Audience is the aud claim OpenAI's token exchange requires on a Google
	// ID token.
	Audience = "https://api.openai.com/v1"
)

// Config identifies the OpenAI workload identity federation to use. Both IDs
// are required: the token exchange rejects a request without either.
type Config struct {
	// IdentityProviderID is the OpenAI workload identity provider (idp_*)
	// that trusts the Google issuer.
	IdentityProviderID string
	// ServiceAccountID is the OpenAI service account the exchanged token acts
	// as.
	ServiceAccountID string
	// BaseURL overrides DefaultBaseURL for API requests. A project with data
	// residency must use its regional host (https://us.api.openai.com/v1 for
	// US residency): api.openai.com rejects its tokens as incorrect_hostname.
	// The token exchange endpoint is the same for every project.
	BaseURL string
}

// APIBaseURL returns BaseURL, or DefaultBaseURL when it is empty.
func (c Config) APIBaseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

// Validate reports whether both IDs are set and free of surrounding
// whitespace, and whether BaseURL, when set, is an absolute HTTPS URL: the
// exchanged bearer is sent to it.
func (c Config) Validate() error {
	var errs []error
	for _, field := range []struct{ name, value string }{
		{"identity provider ID", c.IdentityProviderID},
		{"service account ID", c.ServiceAccountID},
	} {
		switch {
		case field.value == "":
			errs = append(errs, fmt.Errorf("OpenAI workload identity requires a %s", field.name))
		case field.value != strings.TrimSpace(field.value):
			errs = append(errs, fmt.Errorf("OpenAI %s must not contain leading or trailing whitespace", field.name))
		}
	}
	if c.BaseURL != "" {
		if u, err := url.Parse(c.BaseURL); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			errs = append(errs, fmt.Errorf("OpenAI base URL %q must be an absolute https URL without credentials", c.BaseURL))
		}
	}
	return errors.Join(errs...)
}

// WorkloadIdentity returns the SDK configuration for
// option.WithWorkloadIdentity. Subject tokens are Google ID tokens for
// Audience from Application Default Credentials, which are discovered on the
// first token exchange rather than here.
func (c Config) WorkloadIdentity() (auth.WorkloadIdentity, error) {
	if err := c.Validate(); err != nil {
		return auth.WorkloadIdentity{}, err
	}
	return auth.WorkloadIdentity{
		IdentityProviderID: c.IdentityProviderID,
		ServiceAccountID:   c.ServiceAccountID,
		Provider:           &googleIDTokenProvider{audience: Audience},
	}, nil
}

// googleIDTokenProvider supplies Google ID tokens as the exchange's subject
// token. The token source is built once and reused, so its cached ID token is
// reused until it nears expiry.
type googleIDTokenProvider struct {
	audience string
	// timeout bounds each HTTP request of the oauth2 key-file JWT flow; zero
	// means defaultFetchTimeout. On GCE (GKE, Cloud Run) the token comes from
	// the metadata server, whose own client bounds each request to 5 seconds
	// and this field has no effect.
	timeout time.Duration

	mu       sync.Mutex
	source   oauth2.TokenSource
	inflight *idTokenFetch
}

// idTokenFetch is one fetch that every concurrent GetToken waits on.
type idTokenFetch struct {
	done  chan struct{}
	token string
	err   error
}

// defaultFetchTimeout bounds each request of the key-file JWT flow to Google's
// token endpoint, so a stalled endpoint fails the fetch and the next GetToken
// starts a fresh one.
const defaultFetchTimeout = 30 * time.Second

var _ auth.SubjectTokenProvider = (*googleIDTokenProvider)(nil)

// TokenType reports an OIDC ID token, which the exchange sends as
// urn:ietf:params:oauth:token-type:id_token.
func (*googleIDTokenProvider) TokenType() auth.SubjectTokenType {
	return auth.SubjectTokenTypeID
}

// GetToken returns a Google ID token for the provider's audience. The SDK's
// HTTP client is not used: Application Default Credentials own their own
// transport.
//
// oauth2.TokenSource.Token takes no context, so the fetch runs on its own
// goroutine, shared by every concurrent caller, and GetToken returns as soon
// as ctx ends. A fetch the caller abandons still completes, bounded by the
// provider's timeout, and fills the source's cache for the next call.
func (p *googleIDTokenProvider) GetToken(ctx context.Context, _ auth.HTTPDoer) (string, error) {
	p.mu.Lock()
	fetch := p.inflight
	if fetch == nil {
		fetch = &idTokenFetch{done: make(chan struct{})}
		p.inflight = fetch
		go p.run(context.WithoutCancel(ctx), fetch)
	}
	p.mu.Unlock()
	select {
	case <-fetch.done:
		return fetch.token, fetch.err
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for a Google ID token: %w", ctx.Err())
	}
}

// run performs fetch and then clears it, so the next GetToken starts anew.
func (p *googleIDTokenProvider) run(ctx context.Context, fetch *idTokenFetch) {
	fetch.token, fetch.err = p.token(ctx)
	p.mu.Lock()
	p.inflight = nil
	p.mu.Unlock()
	close(fetch.done)
}

// token runs on one fetch goroutine at a time, so it alone touches source.
func (p *googleIDTokenProvider) token(ctx context.Context) (string, error) {
	if p.source == nil {
		timeout := p.timeout
		if timeout == 0 {
			timeout = defaultFetchTimeout
		}
		// The source outlives this exchange and refreshes its token on later
		// ones, so ctx carries no request's cancellation; the client's timeout
		// bounds each of its requests instead. Only the oauth2 key-file JWT
		// flow takes its client from this key. On GCE both idtoken libraries
		// fetch from the metadata server with the metadata package's own
		// client (5 seconds per request, up to 5 retries), and the newer auth
		// library's service-account path
		// (GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB) uses its own
		// client with a 30-second timeout matching defaultFetchTimeout.
		// Neither path accepts a client, so a non-default timeout applies only
		// to the oauth2 key-file flow.
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: timeout})
		source, err := idtoken.NewTokenSource(ctx, p.audience)
		if err != nil {
			return "", fmt.Errorf("creating Google ID-token source for audience %q: %w", p.audience, err)
		}
		p.source = source
	}
	token, err := p.source.Token()
	if err != nil {
		return "", fmt.Errorf("fetching Google ID token: %w", err)
	}
	// idtoken sources carry the OIDC JWT in AccessToken.
	if token.AccessToken == "" {
		return "", errors.New("empty token from the Google ID-token source")
	}
	return token.AccessToken, nil
}
