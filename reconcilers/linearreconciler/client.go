/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"bytes"
	"context"
	"encoding/json"
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
	// Refresh 30 seconds early to avoid edge-case expiry.
	c.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - 30*time.Second)
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
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return fmt.Errorf("unmarshaling response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
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
			id identifier title description updatedAt
			state { name type }
			team { key name }
			assignee { id name }
			labels { nodes { name } }
			attachments { nodes { id title subtitle url } }
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

// UploadFileAttachment uploads content as a file attachment on an issue.
// If existingID is non-empty, the old attachment is deleted first (best-effort).
func (c *Client) UploadFileAttachment(ctx context.Context, issueID, title string, content []byte, existingID string) error {
	// Delete old attachment if updating.
	if existingID != "" {
		const deleteMutation = `mutation($id: String!) {
			attachmentDelete(id: $id) { success }
		}`
		var deleteResult struct {
			AttachmentDelete struct {
				Success bool `json:"success"`
			} `json:"attachmentDelete"`
		}
		_ = c.graphql(ctx, deleteMutation, map[string]any{"id": existingID}, &deleteResult)
	}

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

// FetchAttachmentContent downloads the content of a file attachment by URL.
// The API token is only sent to trusted Linear hosts (*.linear.app) over HTTPS
// to prevent credential leakage if an attacker controls the attachment URL.
func (c *Client) FetchAttachmentContent(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing attachment URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	if parsed.Scheme == "https" && isLinearHost(parsed.Hostname()) {
		token, err := c.getToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("getting access token: %w", err)
		}
		if c.isAPIKey {
			req.Header.Set("Authorization", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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
