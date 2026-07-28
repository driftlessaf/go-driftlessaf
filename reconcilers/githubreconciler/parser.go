/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseURL parses a GitHub resource URL and extracts the components.
//
// Expected formats:
//   - https://github.com/org/repo/issues/123
//   - https://github.com/org/repo/pull/123
//   - https://github.com/org/repo/blob/ref/path/to/file
//   - https://github.com/org/repo/tree/ref/path/to/dir
//
// A scheme-relative key (e.g. "github.com/org/repo/tree/ref/dir", with no
// "https://") is also accepted: the default scheme is assumed for parsing so
// workqueue keys stored without a scheme resolve to the same resource. The
// original string is preserved verbatim in Resource.URL so the key round-trips.
func ParseURL(uri string) (*Resource, error) {
	// Assume https when the key carries no scheme. url.Parse only populates Host
	// when a scheme (and thus "//" authority) is present, so a bare
	// "github.com/…" would otherwise land entirely in Path and fail the host
	// check below.
	parseTarget := uri
	if !hasScheme(uri) {
		parseTarget = "https://" + uri
	}

	parsed, err := url.Parse(parseTarget)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	// Validate host
	if parsed.Host != "github.com" {
		return nil, fmt.Errorf("invalid host: %s (expected github.com)", parsed.Host)
	}

	// Split path into components
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid path format: %s", parsed.Path)
	}

	owner := parts[0]
	repo := parts[1]
	resourceType := parts[2]

	switch resourceType {
	case "issues", "pull":
		if len(parts) != 4 {
			return nil, fmt.Errorf("invalid path format: %s", parsed.Path)
		}
		number, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, fmt.Errorf("invalid resource number: %s", parts[3])
		}

		var resType ResourceType
		if resourceType == "issues" {
			resType = ResourceTypeIssue
		} else {
			resType = ResourceTypePullRequest
		}

		return &Resource{
			Owner:  owner,
			Repo:   repo,
			Number: number,
			Type:   resType,
			URL:    uri,
		}, nil

	case "blob", "tree":
		if len(parts) < 5 {
			return nil, fmt.Errorf("invalid path format: %s", parsed.Path)
		}
		ref := parts[3]
		filePath := strings.Join(parts[4:], "/")
		return &Resource{
			Owner: owner,
			Repo:  repo,
			Type:  ResourceTypePath,
			URL:   uri,
			Ref:   ref,
			Path:  filePath,
		}, nil

	default:
		return nil, fmt.Errorf("unknown resource type: %s", resourceType)
	}
}

// hasScheme reports whether uri begins with a URL scheme (e.g. "https://"). It is
// deliberately strict: the text before "://" must contain no "/" or "." so a
// scheme-relative key like "github.com/org/repo" is not mistaken for one.
func hasScheme(uri string) bool {
	i := strings.Index(uri, "://")
	return i > 0 && !strings.ContainsAny(uri[:i], "/.")
}
