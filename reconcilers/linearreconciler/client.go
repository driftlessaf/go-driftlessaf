/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultEndpoint is the Linear GraphQL API endpoint.
	DefaultEndpoint = "https://api.linear.app/graphql"

	// DefaultTokenURL is the Linear OAuth token endpoint.
	DefaultTokenURL = "https://api.linear.app/oauth/token" //nolint:gosec // This is a URL, not a credential.

	// maxGraphQLResponseSize caps GraphQL API response reads (10 MB).
	maxGraphQLResponseSize = 10 << 20
	// maxAttachmentSize caps downloaded attachment reads (10 MB).
	maxAttachmentSize = 10 << 20
	// maxErrorBodySize caps error response bodies included in error messages (1 KB).
	maxErrorBodySize = 1 << 10

	// maxTokenCacheTTL caps how long an issued access token is cached
	// locally before getToken() forces a refresh, regardless of the
	// `expires_in` value Linear advertises.
	//
	// Background: a long-running bot was observed running for ~22 hours
	// on a single cached token. Linear's token endpoint returned
	// expires_in ≈ 30 days, which getToken() trusted, so the local
	// tokenExpiry was set 30 days out and the cache was never refreshed.
	// Around the 22-hour mark every GraphQL request started returning
	// HTTP 401 "Authentication required, not authenticated" — the token
	// was being rejected on use even though the local cache still
	// considered it valid. A manual client_credentials exchange against
	// the same OAuth app produced a fresh token that worked immediately,
	// confirming the credentials and the OAuth app were healthy and the
	// failure was specifically in the cache-versus-server-side-TTL skew.
	// Restarting the bot process (which reset the cache) restored service.
	//
	// Linear's effective server-side token TTL is undocumented and
	// clearly shorter than the advertised expires_in. Capping the local
	// cache at 6h gives a safe lower bound well below the observed
	// failure point, keeps refresh traffic to the token endpoint modest
	// (~4 refreshes/day per process), and bounds how long any individual
	// stale-token outage can last to roughly the cap.
	maxTokenCacheTTL = 6 * time.Hour
)

// OAuth scope constants for Linear API permissions.
const (
	ScopeRead           = "read"
	ScopeWrite          = "write"
	ScopeIssuesCreate   = "issues:create"
	ScopeCommentsCreate = "comments:create"
)

// sensitiveHeaders must not be forwarded from API-provided header lists.
var sensitiveHeaders = map[string]struct{}{
	"Authorization":       {},
	"Cookie":              {},
	"Host":                {},
	"Proxy-Authorization": {},
}

// RateLimitError is returned when the Linear API returns HTTP 429.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("linear rate limited, retry after %v", e.RetryAfter)
}

// Client is a Linear GraphQL API client that supports two authentication modes:
//
// OAuth client_credentials (recommended for production):
//
//	client := linearreconciler.NewClient(clientID, clientSecret)
//
// OAuth is preferred because it issues short-lived access tokens that are
// automatically refreshed, limits the blast radius of a compromised credential,
// and allows fine-grained permission scoping via WithScopes.
//
// Static API key (acceptable for development and testing):
//
//	client := linearreconciler.NewClientWithAPIKey(apiKey)
//
// API keys are long-lived and grant the full permissions of the user who
// created them. They should only be used for local development or tests.
type Client struct {
	clientID     string
	clientSecret string
	scopes       []string
	endpoint     string
	tokenURL     string
	httpClient   *http.Client
	statePrefix  string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	isAPIKey    bool

	// BotUserID is the authenticated user's ID, resolved during reconciler construction.
	BotUserID string
}

// NewClient creates a new Linear client that uses OAuth client_credentials.
// This is the recommended constructor for production use because:
//   - Tokens are short-lived and automatically refreshed, reducing exposure
//     if a token is leaked.
//   - Permissions are scoped to only what the bot needs (see WithScopes),
//     rather than inheriting the full permissions of a user account.
//   - Client credentials can be rotated independently of any user account.
//
// By default, the client requests ScopeRead and ScopeWrite. Use WithScopes
// to request only the permissions your reconciler needs.
func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		scopes:       []string{ScopeRead, ScopeWrite},
		endpoint:     DefaultEndpoint,
		tokenURL:     DefaultTokenURL,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		statePrefix:  "reconciler",
	}
}

// NewClientWithAPIKey creates a new Linear client using a static API key.
// API keys are long-lived and carry the full permissions of the creating user,
// so prefer NewClient with OAuth client_credentials for production deployments.
func NewClientWithAPIKey(apiKey string) *Client {
	return &Client{
		endpoint:    DefaultEndpoint,
		tokenURL:    DefaultTokenURL,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		statePrefix: "reconciler",
		token:       apiKey,
		tokenExpiry: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		isAPIKey:    true,
	}
}

// WithScopes sets the OAuth scopes requested during token exchange.
// Use this to restrict the bot to only the permissions it needs. For example,
// a read-only reconciler should use WithScopes(ScopeRead).
func (c *Client) WithScopes(scopes ...string) *Client {
	c.scopes = scopes
	return c
}

// WithEndpoint sets a custom API endpoint (useful for testing).
func (c *Client) WithEndpoint(endpoint string) *Client {
	c.endpoint = endpoint
	return c
}

// WithTokenURL sets a custom OAuth token endpoint (useful for testing).
func (c *Client) WithTokenURL(tokenURL string) *Client {
	c.tokenURL = tokenURL
	return c
}

// WithHTTPClient sets a custom HTTP client.
func (c *Client) WithHTTPClient(httpClient *http.Client) *Client {
	c.httpClient = httpClient
	return c
}

// getToken returns a valid access token, fetching or refreshing as needed.
func (c *Client) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("scope", strings.Join(c.scopes, ","))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGraphQLResponseSize))
	if err != nil {
		return "", fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody := body
		if len(errBody) > maxErrorBodySize {
			errBody = errBody[:maxErrorBodySize]
		}
		return "", fmt.Errorf("token request failed: status=%d body=%s", resp.StatusCode, errBody)
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token")
	}

	c.token = tr.AccessToken
	// Cache lifetime is min(advertised expires_in, maxTokenCacheTTL),
	// minus a 30s safety buffer to avoid edge-case expiry. The cap
	// protects against the documented case where Linear advertises a
	// multi-week expires_in but invalidates the token server-side much
	// sooner; see the maxTokenCacheTTL comment for the incident detail.
	advertised := min(time.Duration(tr.ExpiresIn)*time.Second, maxTokenCacheTTL)
	c.tokenExpiry = time.Now().Add(advertised - 30*time.Second)
	return c.token, nil
}

func (c *Client) graphql(ctx context.Context, query string, variables map[string]any, result any) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return fmt.Errorf("getting access token: %w", err)
	}

	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return fmt.Errorf("marshaling query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.isAPIKey {
		req.Header.Set("Authorization", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxGraphQLResponseSize))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := time.Minute
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if seconds, err := strconv.Atoi(ra); err == nil {
				retryAfter = time.Duration(seconds) * time.Second
			}
		}
		return &RateLimitError{RetryAfter: retryAfter}
	}

	if resp.StatusCode != http.StatusOK {
		errBody := respBody
		if len(errBody) > maxErrorBodySize {
			errBody = errBody[:maxErrorBodySize]
		}
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, errBody)
	}

	var gqlResp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string         `json:"message"`
			Path       []any          `json:"path,omitempty"`
			Extensions map[string]any `json:"extensions,omitempty"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return fmt.Errorf("unmarshaling response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		// Linear's "Argument Validation Error" only tells you something
		// failed — the actual offending field/value is in extensions
		// (e.g. extensions.userPresentableMessage, extensions.code).
		// Include the whole extensions blob so callers can see *what*
		// was rejected without having to mirror an HTTP capture.
		first := gqlResp.Errors[0]
		details := first.Message
		if first.Extensions != nil {
			if extJSON, mErr := json.Marshal(first.Extensions); mErr == nil {
				details = fmt.Sprintf("%s (extensions=%s)", details, extJSON)
			}
		}
		return fmt.Errorf("graphql error: %s", details)
	}

	return json.Unmarshal(gqlResp.Data, result)
}

// GetViewer returns the authenticated user.
func (c *Client) GetViewer(ctx context.Context) (*User, error) {
	var result struct {
		Viewer User `json:"viewer"`
	}
	err := c.graphql(ctx, `query { viewer { id name } }`, nil, &result)
	if err != nil {
		return nil, err
	}
	return &result.Viewer, nil
}

// GetIssue fetches an issue by ID, including comments, labels, and attachments.
func (c *Client) GetIssue(ctx context.Context, issueID string) (*Issue, error) {
	const query = `query($id: String!) {
		issue(id: $id) {
			id identifier title description updatedAt url
			state { name type }
			team { id key name }
			assignee { id name }
			creator { id name app }
			labels { nodes { name } }
			documents { nodes { id slugId title url } }
			attachments { nodes { id title subtitle url createdAt } }
			comments(first: 100, orderBy: createdAt) {
				nodes {
					id body createdAt
					user { id name }
				}
			}
		}
	}`

	var result struct {
		Issue *Issue `json:"issue"`
	}
	err := c.graphql(ctx, query, map[string]any{"id": issueID}, &result)
	if err != nil {
		return nil, err
	}
	if result.Issue == nil {
		return nil, fmt.Errorf("issue %s not found", issueID)
	}

	sort.Slice(result.Issue.Comments.Nodes, func(i, j int) bool {
		return result.Issue.Comments.Nodes[i].CreatedAt.Before(result.Issue.Comments.Nodes[j].CreatedAt)
	})

	return result.Issue, nil
}

// CreateComment posts a comment on an issue.
func (c *Client) CreateComment(ctx context.Context, issueID, body string) error {
	const mutation = `mutation($issueId: String!, $body: String!) {
		commentCreate(input: { issueId: $issueId, body: $body }) {
			success
		}
	}`

	var result struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	err := c.graphql(ctx, mutation, map[string]any{
		"issueId": issueID,
		"body":    body,
	}, &result)
	if err != nil {
		return err
	}
	if !result.CommentCreate.Success {
		return fmt.Errorf("comment creation failed")
	}
	return nil
}

// UpdateComment updates an existing comment's body.
func (c *Client) UpdateComment(ctx context.Context, commentID, body string) error {
	const mutation = `mutation($id: String!, $body: String!) {
		commentUpdate(id: $id, input: { body: $body }) {
			success
		}
	}`

	var result struct {
		CommentUpdate struct {
			Success bool `json:"success"`
		} `json:"commentUpdate"`
	}
	err := c.graphql(ctx, mutation, map[string]any{
		"id":   commentID,
		"body": body,
	}, &result)
	if err != nil {
		return err
	}
	if !result.CommentUpdate.Success {
		return fmt.Errorf("comment update failed")
	}
	return nil
}

// upsertComment creates or updates a comment on an issue.
// If commentID is non-empty, the existing comment is updated.
// Otherwise, a new comment is created. Returns the comment ID.
func (c *Client) upsertComment(ctx context.Context, issueID, commentID, body string) (string, error) {
	if commentID != "" {
		if err := c.UpdateComment(ctx, commentID, body); err != nil {
			return commentID, err
		}
		return commentID, nil
	}

	const mutation = `mutation($issueId: String!, $body: String!) {
		commentCreate(input: { issueId: $issueId, body: $body }) {
			success
			comment { id }
		}
	}`

	var result struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	err := c.graphql(ctx, mutation, map[string]any{
		"issueId": issueID,
		"body":    body,
	}, &result)
	if err != nil {
		return "", err
	}
	if !result.CommentCreate.Success {
		return "", fmt.Errorf("comment creation failed")
	}
	return result.CommentCreate.Comment.ID, nil
}

// UpdateIssueDescription updates the issue's description.
func (c *Client) UpdateIssueDescription(ctx context.Context, issueID, description string) error {
	const mutation = `mutation($id: String!, $description: String!) {
		issueUpdate(id: $id, input: { description: $description }) {
			success
		}
	}`

	var result struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	return c.graphql(ctx, mutation, map[string]any{
		"id":          issueID,
		"description": description,
	}, &result)
}

// SetIssueStateByType moves a Linear issue to its team's workflow state
// matching stateType. State NAMES are workspace-renameable ("Done" →
// "Resolved"); the schema-stable types are: backlog, unstarted, started,
// completed, canceled, triage.
//
// preferredNames disambiguates when a team has multiple workflow states
// of the same type — most commonly "canceled", which Linear uses for
// both the "Cancelled"/"Canceled" and "Duplicate" UI states. The lookup
// picks the first state whose type matches AND whose name
// (case-insensitive) appears in preferredNames; if no name preference
// matches, falls back to the first state by type alone (preserving the
// workspace-rename-survival guarantee for callers that don't care about
// disambiguation).
//
// Looks up the target state ID via the issue's team in a single GraphQL
// round-trip, then issues the update. Returns nil with no error if the
// team has no state of the requested type — that's a workspace-config
// gap rather than a code bug, and silently no-oping keeps event
// handlers from dying on workspaces that haven't set up the expected
// workflow.
func (c *Client) SetIssueStateByType(ctx context.Context, issueID, stateType string, preferredNames ...string) error {
	const lookup = `query($id: String!) {
		issue(id: $id) {
			id
			team {
				states {
					nodes { id name type }
				}
			}
		}
	}`

	var lookupResult struct {
		Issue *struct {
			ID   string `json:"id"`
			Team struct {
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := c.graphql(ctx, lookup, map[string]any{"id": issueID}, &lookupResult); err != nil {
		return fmt.Errorf("look up team states for issue %s: %w", issueID, err)
	}
	if lookupResult.Issue == nil {
		return fmt.Errorf("issue %s not found", issueID)
	}

	preferredSet := make(map[string]struct{}, len(preferredNames))
	for _, n := range preferredNames {
		preferredSet[strings.ToLower(n)] = struct{}{}
	}

	var preferredID, fallbackID string
	for _, s := range lookupResult.Issue.Team.States.Nodes {
		if s.Type != stateType {
			continue
		}
		if fallbackID == "" {
			fallbackID = s.ID
		}
		if _, ok := preferredSet[strings.ToLower(s.Name)]; ok {
			preferredID = s.ID
			break // exact preferred match — stop searching
		}
	}
	stateID := preferredID
	if stateID == "" {
		stateID = fallbackID
	}
	if stateID == "" {
		// No state of the requested type on this team. Likely a workspace
		// that's renamed/removed the default state; not a bug worth failing
		// the whole event handler over.
		return nil
	}

	const mutation = `mutation($id: String!, $stateId: String!) {
		issueUpdate(id: $id, input: { stateId: $stateId }) {
			success
		}
	}`
	var mutResult struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{
		"id":      issueID,
		"stateId": stateID,
	}, &mutResult); err != nil {
		return fmt.Errorf("set issue %s state to %s: %w", issueID, stateType, err)
	}
	if !mutResult.IssueUpdate.Success {
		return fmt.Errorf("set issue %s state to %s: API returned success=false", issueID, stateType)
	}
	return nil
}

// FindStateIDByType returns the workflow state UUID on the given team
// matching stateType, optionally disambiguated by preferredNames.
//
// Mirrors the lookup-half of SetIssueStateByType so callers can resolve
// a state ID once per team and reuse it across many writes (e.g. setting
// the initial state on a batch of newly-created child issues without
// issuing N follow-up update mutations). Returns "" with no error when
// the team has no state of the requested type — this matches
// SetIssueStateByType's "missing-config is not a code bug" behavior.
//
// stateType is the schema-stable Linear workflow type ("backlog",
// "unstarted", "started", "completed", "canceled", "triage"). Names
// like "Todo"/"In Progress" are workspace-renameable; pass them via
// preferredNames only as disambiguation, not as the primary key.
func (c *Client) FindStateIDByType(ctx context.Context, teamID, stateType string, preferredNames ...string) (string, error) {
	const lookup = `query($id: String!) {
		team(id: $id) {
			states {
				nodes { id name type }
			}
		}
	}`
	var result struct {
		Team *struct {
			States struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	if err := c.graphql(ctx, lookup, map[string]any{"id": teamID}, &result); err != nil {
		return "", fmt.Errorf("look up states for team %s: %w", teamID, err)
	}
	if result.Team == nil {
		return "", fmt.Errorf("team %s not found", teamID)
	}

	preferredSet := make(map[string]struct{}, len(preferredNames))
	for _, n := range preferredNames {
		preferredSet[strings.ToLower(n)] = struct{}{}
	}

	var preferredID, fallbackID string
	for _, s := range result.Team.States.Nodes {
		if s.Type != stateType {
			continue
		}
		if fallbackID == "" {
			fallbackID = s.ID
		}
		if _, ok := preferredSet[strings.ToLower(s.Name)]; ok {
			preferredID = s.ID
			break
		}
	}
	if preferredID != "" {
		return preferredID, nil
	}
	return fallbackID, nil
}

// DeleteAttachment deletes an attachment by ID.
func (c *Client) DeleteAttachment(ctx context.Context, attachmentID string) error {
	const mutation = `mutation($id: String!) {
		attachmentDelete(id: $id) { success }
	}`
	var result struct {
		AttachmentDelete struct {
			Success bool `json:"success"`
		} `json:"attachmentDelete"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{"id": attachmentID}, &result); err != nil {
		return fmt.Errorf("deleting attachment %s: %w", attachmentID, err)
	}
	if !result.AttachmentDelete.Success {
		return fmt.Errorf("deleting attachment %s: API returned success=false", attachmentID)
	}
	return nil
}

// UploadFileAttachment uploads content as a file attachment on an issue.
func (c *Client) UploadFileAttachment(ctx context.Context, issueID, title string, content []byte) error {
	// Step 1: Request a presigned upload URL.
	const uploadMutation = `mutation($contentType: String!, $filename: String!, $size: Int!) {
		fileUpload(contentType: $contentType, filename: $filename, size: $size) {
			uploadFile {
				uploadUrl
				assetUrl
				headers { key value }
			}
		}
	}`
	var uploadResult struct {
		FileUpload struct {
			UploadFile struct {
				UploadURL string `json:"uploadUrl"`
				AssetURL  string `json:"assetUrl"`
				Headers   []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"uploadFile"`
		} `json:"fileUpload"`
	}
	if err := c.graphql(ctx, uploadMutation, map[string]any{
		"contentType": "application/json",
		"filename":    title + ".json",
		"size":        len(content),
	}, &uploadResult); err != nil {
		return fmt.Errorf("requesting upload URL: %w", err)
	}

	// Step 2: PUT the content to the presigned URL.
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadResult.FileUpload.UploadFile.UploadURL, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("creating PUT request: %w", err)
	}
	putReq.Header.Set("Content-Type", "application/json")
	for _, h := range uploadResult.FileUpload.UploadFile.Headers {
		if _, ok := sensitiveHeaders[http.CanonicalHeaderKey(h.Key)]; ok {
			continue
		}
		putReq.Header.Set(h.Key, h.Value)
	}
	putResp, err := c.httpClient.Do(putReq)
	if err != nil {
		return fmt.Errorf("uploading file: %w", err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(putResp.Body, maxErrorBodySize))
		return fmt.Errorf("upload failed with status %d: %s", putResp.StatusCode, respBody)
	}

	// Step 3: Create an attachment linking to the uploaded file.
	const attachMutation = `mutation($issueId: String!, $title: String!, $url: String!) {
		attachmentCreate(input: { issueId: $issueId, title: $title, url: $url }) {
			success
		}
	}`
	var attachResult struct {
		AttachmentCreate struct {
			Success bool `json:"success"`
		} `json:"attachmentCreate"`
	}
	if err := c.graphql(ctx, attachMutation, map[string]any{
		"issueId": issueID,
		"title":   title,
		"url":     uploadResult.FileUpload.UploadFile.AssetURL,
	}, &attachResult); err != nil {
		return fmt.Errorf("creating attachment: %w", err)
	}

	return nil
}

// isLinearHost returns true if the host is a trusted Linear domain.
func isLinearHost(host string) bool {
	return host == "linear.app" || strings.HasSuffix(host, ".linear.app")
}

// isAllowedAttachmentURL gates which URLs FetchAttachmentContent will fetch.
// In production this resolves to HTTPS Linear hosts only. Tests that point
// the client at a mock server via WithEndpoint can fetch attachments from
// that same server (matching scheme + host) without needing to weaken the
// production allowlist.
func (c *Client) isAllowedAttachmentURL(parsed *url.URL) bool {
	if parsed.Scheme == "https" && isLinearHost(parsed.Hostname()) {
		return true
	}
	endpointURL, err := url.Parse(c.endpoint)
	if err != nil {
		return false
	}
	return endpointURL.Scheme == parsed.Scheme && endpointURL.Host == parsed.Host
}

// FetchAttachmentContent downloads the content of a file attachment by URL.
// To avoid Server-Side Request Forgery, only HTTPS URLs pointing at trusted
// Linear hosts (*.linear.app), or URLs sharing the configured endpoint's
// scheme+host, are accepted. Attachment URLs are attacker-controllable in
// principle, so an unrestricted fetch could be coerced into reaching
// internal services or cloud metadata endpoints.
func (c *Client) FetchAttachmentContent(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing attachment URL: %w", err)
	}
	if !c.isAllowedAttachmentURL(parsed) {
		return nil, fmt.Errorf("attachment URL %q is not an allowed host (allowed: *.linear.app or the configured endpoint)", rawURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting access token: %w", err)
	}
	if c.isAPIKey {
		req.Header.Set("Authorization", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching attachment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, maxAttachmentSize))
}

// Document represents a Linear document.
type Document struct {
	ID        string
	Slug      string
	Title     string
	Content   string
	URL       string
	IssueID   string
	ProjectID string
}

// ErrDocumentNotFound is returned by GetDocument when Linear has no document
// with the requested id/slug.
var ErrDocumentNotFound = errors.New("document not found")

// GetDocument fetches a Linear document by id or slug.
// Returns ErrDocumentNotFound if Linear returns null for the document.
func (c *Client) GetDocument(ctx context.Context, idOrSlug string) (Document, error) {
	const query = `query($id: String!) {
		document(id: $id) {
			id
			slugId
			title
			content
			url
			issue { id }
			project { id }
		}
	}`

	var result struct {
		Document *struct {
			ID      string `json:"id"`
			SlugID  string `json:"slugId"`
			Title   string `json:"title"`
			Content string `json:"content"`
			URL     string `json:"url"`
			Issue   *struct {
				ID string `json:"id"`
			} `json:"issue"`
			Project *struct {
				ID string `json:"id"`
			} `json:"project"`
		} `json:"document"`
	}

	if err := c.graphql(ctx, query, map[string]any{"id": idOrSlug}, &result); err != nil {
		return Document{}, fmt.Errorf("query document: %w", err)
	}
	if result.Document == nil {
		return Document{}, ErrDocumentNotFound
	}

	doc := Document{
		ID:      result.Document.ID,
		Slug:    result.Document.SlugID,
		Title:   result.Document.Title,
		Content: result.Document.Content,
		URL:     result.Document.URL,
	}
	if result.Document.Issue != nil {
		doc.IssueID = result.Document.Issue.ID
	}
	if result.Document.Project != nil {
		doc.ProjectID = result.Document.Project.ID
	}
	return doc, nil
}
