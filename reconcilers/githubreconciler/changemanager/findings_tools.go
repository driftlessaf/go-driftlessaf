/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/google/go-github/v75/github"
)

const (
	// maxLogRedirects is the maximum number of redirects to follow when fetching logs.
	maxLogRedirects = 2
)

var (
	// actionsURLRegex matches GitHub Actions workflow run and job IDs from details URLs.
	// Example: https://github.com/{owner}/{repo}/actions/runs/{run_id}/job/{job_id}
	actionsURLRegex = regexp.MustCompile(`/actions/runs/(\d+)/job/(\d+)`)

	// timestampRegex matches timestamps at the beginning of log lines.
	// Example: "2024-01-15T10:30:45.1234567Z "
	// The (?m) flag enables multiline mode where ^ matches start of each line.
	timestampRegex = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z ?`)

	// ansiRegex matches ANSI color codes in logs.
	ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// getGitHubActionsLogs fetches logs from GitHub Actions for a specific job.
func getGitHubActionsLogs(ctx context.Context, gh *github.Client, owner, repo string, jobID int64) (string, error) {
	// Get job logs URL
	jobLogsURL, _, err := gh.Actions.GetWorkflowJobLogs(ctx, owner, repo, jobID, maxLogRedirects)
	if err != nil {
		return "", fmt.Errorf("failed to get workflow job logs URL: %w", err)
	}

	// Download and process the logs
	return downloadLogs(ctx, jobLogsURL.String())
}

// downloadLogs downloads logs from a URL and cleans them.
func downloadLogs(ctx context.Context, url string) (string, error) {
	// Create request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Create HTTP client with timeout and execute request: GH Action Log URLs
	// are Azure Storage URLs with token baked in the URL, so we can not use
	// an authenticated client here as the Auth header will cause 400.
	resp, err := (&http.Client{
		Timeout: 30 * time.Second,
	}).Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download logs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download logs: status %d", resp.StatusCode)
	}

	// Limit log size to 1MB to avoid overloading the AI context window and OOMing.
	const maxLogSize = 1 << 20 // 1 MiB
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxLogSize))
	if err != nil {
		return "", fmt.Errorf("failed to read logs: %w", err)
	}

	logs := string(content)

	// Clean up logs - remove timestamps and ANSI codes
	logs = cleanLogs(logs)

	return logs, nil
}

// cleanLogs removes timestamps and ANSI color codes from logs.
func cleanLogs(logs string) string {
	logs = timestampRegex.ReplaceAllString(logs, "")
	logs = ansiRegex.ReplaceAllString(logs, "")
	return logs
}

// fetchFindingLogs fetches logs for a finding based on its details URL.
// Currently supports GitHub Actions logs; other log sources return an error.
func fetchFindingLogs(ctx context.Context, gh *github.Client, owner, repo, detailsURL string) (string, error) {
	if detailsURL == "" {
		return "", fmt.Errorf("no details URL available for finding")
	}

	// Check if it's a GitHub Actions URL
	if matches := actionsURLRegex.FindStringSubmatch(detailsURL); len(matches) > 2 {
		jobID, err := strconv.ParseInt(matches[2], 10, 64)
		if err != nil {
			return "", fmt.Errorf("parse job ID from URL %q: %w", detailsURL, err)
		}
		if jobID != 0 {
			return getGitHubActionsLogs(ctx, gh, owner, repo, jobID)
		}
	}

	return "", fmt.Errorf("unsupported log source: %s", detailsURL)
}
