/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"text/template"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	internaltemplate "chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v84/github"
	"github.com/shurcooL/githubv4"
)

// Option configures a CM (ChangeManager).
type Option[T any] func(*CM[T])

// WithOwner overrides the GitHub owner (org or user) from the resource.
// When set, all PR operations will use this owner instead of the resource's owner.
func WithOwner[T any](owner string) Option[T] {
	return func(cm *CM[T]) {
		cm.owner = owner
	}
}

// WithRepo overrides the GitHub repository from the resource.
// When set, all PR operations will use this repo instead of the resource's repo.
func WithRepo[T any](repo string) Option[T] {
	return func(cm *CM[T]) {
		cm.repo = repo
	}
}

// WithFindingsIteration configures the change manager to treat CI findings
// as requiring a refresh. Use this for bots that can iterate on CI failures
// (e.g. via an AI agent). Without this option, CI findings are ignored by
// needsRefresh and Upsert will not re-invoke makeChanges.
func WithFindingsIteration[T any]() Option[T] {
	return func(cm *CM[T]) {
		cm.handlesFindings = true
	}
}

// WithMaxCommits sets the maximum number of commits allowed on a PR before
// the session reports StateMaxCommits. Each commit triggers a CI run, so this
// limits how many times the bot can iterate on a PR. A value of 0 (default)
// means no limit.
func WithMaxCommits[T any](n int) Option[T] {
	return func(cm *CM[T]) {
		cm.maxCommits = n
	}
}

// CM manages the lifecycle of GitHub Pull Requests for a specific identity.
// It uses Go templates to generate PR titles and bodies from generic data of type T.
type CM[T any] struct {
	identity         string
	titleTemplate    *template.Template
	bodyTemplate     *template.Template
	templateExecutor *internaltemplate.Template[T]
	owner            string
	repo             string
	handlesFindings  bool
	maxCommits       int
}

// GraphQL types for querying check runs
type gqlCheckRunNode struct {
	DatabaseId int64
	Name       string
	Status     string
	Conclusion string
	DetailsUrl string
	Title      string
	Summary    string
	Text       string
}

type gqlCheckRunsConnection struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []gqlCheckRunNode
}

// gqlCheckSuiteNode contains filtered check runs for failures and pending checks.
// Using two separate filtered queries is more efficient than fetching all runs.
type gqlCheckSuiteNode struct {
	Id string
	// FailedRuns contains only check runs that concluded with FAILURE
	FailedRuns gqlCheckRunsConnection `graphql:"failedRuns: checkRuns(first: 100, filterBy: {conclusions: [FAILURE]})"`
	// PendingRuns contains check runs that are not yet complete
	PendingRuns gqlCheckRunsConnection `graphql:"pendingRuns: checkRuns(first: 100, filterBy: {statuses: [QUEUED, IN_PROGRESS, WAITING, PENDING, REQUESTED]})"`
}

type gqlCheckSuitesConnection struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []gqlCheckSuiteNode
}

// GraphQL types for querying review threads
type gqlThreadComment struct {
	Author            struct{ Login string }
	AuthorAssociation string
	Body              string
	Url               string
	Commit            struct{ Oid string }
	CreatedAt         string
}

type gqlReviewThread struct {
	Id         string
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       int
	Comments   struct {
		Nodes []gqlThreadComment
	} `graphql:"comments(first: 100)"`
}

type gqlReviewThreadsConnection struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []gqlReviewThread
}

// GraphQL types for querying review bodies (top-level review text only)
type gqlReviewBodyNode struct {
	DatabaseId        int64
	Author            struct{ Login string }
	AuthorAssociation string
	State             string
	Body              string
	Url               string
	SubmittedAt       string
	Commit            struct{ Oid string }
}

type gqlReviewBodiesConnection struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []gqlReviewBodyNode
}

// trustedAuthorAssociations defines which author associations we trust for reviews.
var trustedAuthorAssociations = map[string]struct{}{
	"OWNER":        {},
	"MEMBER":       {},
	"COLLABORATOR": {},
}

// New creates a new CM with the given identity and templates.
// The templates are executed with data of type T when creating or updating PRs.
// Returns an error if titleTemplate or bodyTemplate is nil.
func New[T any](identity string, titleTemplate *template.Template, bodyTemplate *template.Template, opts ...Option[T]) (*CM[T], error) {
	if titleTemplate == nil {
		return nil, errors.New("titleTemplate cannot be nil")
	}
	if bodyTemplate == nil {
		return nil, errors.New("bodyTemplate cannot be nil")
	}

	templateExecutor, err := internaltemplate.New[T](identity, "-pr-data", "PR")
	if err != nil {
		return nil, fmt.Errorf("creating template executor: %w", err)
	}

	cm := &CM[T]{
		identity:         identity,
		titleTemplate:    titleTemplate,
		bodyTemplate:     bodyTemplate,
		templateExecutor: templateExecutor,
	}

	for _, opt := range opts {
		opt(cm)
	}

	return cm, nil
}

// NewSession creates a new Session for the given resource.
// It supports Path and Issue resources, constructing branch names as:
// - Path resources: {identity}/{path}
// - Issue resources: {identity}/issue-{number}
//
// NewSession uses a GraphQL query to fetch PR info and check runs in a single
// request, with pagination for repos with many checks.
func (cm *CM[T]) NewSession(
	ctx context.Context,
	client *github.Client,
	res *githubreconciler.Resource,
) (*Session[T], error) {
	// Determine which owner/repo to use
	owner := res.Owner
	repo := res.Repo
	if cm.owner != "" {
		owner = cm.owner
	}
	if cm.repo != "" {
		repo = cm.repo
	}

	// Construct branch name and ref based on resource type
	var branchName, ref string
	switch res.Type {
	case githubreconciler.ResourceTypePath:
		branchName = cm.identity + "/" + githubreconciler.PathToBranchSuffix(res.Path)
		ref = res.Ref
	case githubreconciler.ResourceTypeIssue:
		branchName = cm.identity + "/issue-" + strconv.Itoa(res.Number)
		ref = "main" // Issues don't have a ref, default to main
	default:
		return nil, fmt.Errorf("change manager only supports Path and Issue resources, got: %v", res.Type)
	}

	// Use GraphQL to fetch PR + check runs in a single query
	gqlClient := graphqlclient.NewGraphQLClient(client)

	var (
		prNumber      int
		prURL         string
		prBody        string
		prMergeable   *bool
		prLabels      []string
		prAssignees   []string
		commitCount   int
		findings      []callbacks.Finding
		pendingChecks []string
	)

	// Initial query for PR and first page of check suites/runs
	var query struct {
		Repository struct {
			PullRequests struct {
				Nodes []struct {
					Number     int
					Url        string
					Body       string
					Mergeable  string // MERGEABLE, CONFLICTING, UNKNOWN
					HeadRefOid string
					Labels     struct {
						Nodes []struct {
							Name string
						}
					} `graphql:"labels(first: 100)"`
					Commits struct {
						TotalCount int
						Nodes      []struct {
							Commit struct {
								CheckSuites gqlCheckSuitesConnection `graphql:"checkSuites(first: 100)"`
							}
						}
					} `graphql:"commits(last: 1)"`
					Assignees struct {
						Nodes []struct {
							Login string
						}
					} `graphql:"assignees(first: 100)"`
					ReviewThreads gqlReviewThreadsConnection `graphql:"reviewThreads(first: 100)"`
					Reviews       gqlReviewBodiesConnection  `graphql:"reviews(first: 100)"`
				}
			} `graphql:"pullRequests(headRefName: $headRef, baseRefName: $baseRef, states: [OPEN], first: 1)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}

	if err := gqlClient.Query(ctx, "GetPRInfo", &query, map[string]any{
		"owner":   githubv4.String(owner),
		"repo":    githubv4.String(repo),
		"headRef": githubv4.String(branchName),
		"baseRef": githubv4.String(ref),
	}); err != nil {
		return nil, fmt.Errorf("querying pull request: %w", err)
	}

	// Process the PR if one exists
	if len(query.Repository.PullRequests.Nodes) > 0 {
		pr := query.Repository.PullRequests.Nodes[0]

		prNumber = pr.Number
		prURL = pr.Url
		prBody = pr.Body
		// Map GraphQL mergeable status to bool pointer
		switch pr.Mergeable {
		case "MERGEABLE":
			prMergeable = ptrTo(true)
		case "CONFLICTING":
			prMergeable = ptrTo(false)
		case "UNKNOWN":
			prMergeable = nil // GitHub is still computing
		}

		// Extract label names
		for _, label := range pr.Labels.Nodes {
			prLabels = append(prLabels, label.Name)
		}

		// Extract assignee logins
		for _, assignee := range pr.Assignees.Nodes {
			prAssignees = append(prAssignees, assignee.Login)
		}

		commitCount = pr.Commits.TotalCount

		// Collect all check runs, handling pagination
		if len(pr.Commits.Nodes) > 0 {
			commit := pr.Commits.Nodes[0].Commit
			findings, pendingChecks = collectFindings(ctx, gqlClient, owner, repo, pr.HeadRefOid, commit.CheckSuites)
		}

		// Collect unresolved review thread findings from trusted authors
		findings = append(findings, collectThreadFindings(ctx, pr.ReviewThreads)...)

		// Collect review body findings from trusted authors on the current commit
		findings = append(findings, collectReviewBodyFindings(ctx, pr.HeadRefOid, pr.Reviews)...)
	}

	return &Session[T]{
		manager:       cm,
		client:        client,
		gqlClient:     gqlClient,
		resource:      res,
		owner:         owner,
		repo:          repo,
		branchName:    branchName,
		ref:           ref,
		prNumber:      prNumber,
		prURL:         prURL,
		prBody:        prBody,
		prMergeable:   prMergeable,
		prLabels:      prLabels,
		prAssignees:   prAssignees,
		commitCount:   commitCount,
		findings:      findings,
		pendingChecks: pendingChecks,
	}, nil
}

func ptrTo[T any](v T) *T {
	return &v
}

// collectThreadFindings extracts findings from unresolved review threads.
// All unresolved threads are included regardless of which commit they were left on.
// Only comments from trusted authors are included; threads with no trusted comments are skipped.
func collectThreadFindings(ctx context.Context, threads gqlReviewThreadsConnection) []callbacks.Finding {
	findings := make([]callbacks.Finding, 0, len(threads.Nodes))

	for _, thread := range threads.Nodes {
		if thread.IsResolved {
			clog.DebugContextf(ctx, "Skipping resolved review thread id=%s path=%s", thread.Id, thread.Path)
			continue
		}

		// Filter to comments from trusted authors only
		var trustedComments []gqlThreadComment
		for _, c := range thread.Comments.Nodes {
			if _, trusted := trustedAuthorAssociations[c.AuthorAssociation]; trusted {
				trustedComments = append(trustedComments, c)
			} else {
				clog.DebugContextf(ctx, "Skipping untrusted thread comment author=%s association=%s thread=%s", c.Author.Login, c.AuthorAssociation, thread.Id)
			}
		}
		if len(trustedComments) == 0 {
			clog.DebugContextf(ctx, "Skipping review thread with no trusted comments id=%s path=%s", thread.Id, thread.Path)
			continue
		}

		findings = append(findings, callbacks.Finding{
			Kind:       callbacks.FindingKindReview,
			Identifier: thread.Id,
			Details:    formatThreadDetails(thread.Path, thread.Line, thread.IsOutdated, trustedComments),
			DetailsURL: trustedComments[0].Url,
		})
	}

	return findings
}

// reviewBodyIdentifierPrefix distinguishes review body findings from thread findings.
const reviewBodyIdentifierPrefix = "review-body:"

// collectReviewBodyFindings extracts findings from non-empty review bodies by trusted
// authors on the current commit. Review bodies lack a resolution concept, so they are
// filtered by commit association: once the bot pushes a new commit, old bodies drop out.
func collectReviewBodyFindings(ctx context.Context, headRefOid string, reviews gqlReviewBodiesConnection) []callbacks.Finding {
	var findings []callbacks.Finding

	for _, review := range reviews.Nodes {
		if _, trusted := trustedAuthorAssociations[review.AuthorAssociation]; !trusted {
			clog.DebugContextf(ctx, "Skipping untrusted review body author=%s association=%s", review.Author.Login, review.AuthorAssociation)
			continue
		}
		if review.Commit.Oid != headRefOid {
			clog.DebugContextf(ctx, "Skipping review body on stale commit author=%s commit=%s head=%s", review.Author.Login, review.Commit.Oid, headRefOid)
			continue
		}
		if review.Body == "" {
			clog.DebugContextf(ctx, "Skipping review body with empty body author=%s", review.Author.Login)
			continue
		}

		findings = append(findings, callbacks.Finding{
			Kind:       callbacks.FindingKindReview,
			Identifier: reviewBodyIdentifierPrefix + fmt.Sprintf("%d", review.DatabaseId),
			Details:    formatReviewBodyDetails(review),
			DetailsURL: review.Url,
		})
	}

	return findings
}

// collectFindings extracts findings and pending checks from check suites, handling pagination.
// Returns findings (failed checks) and pendingChecks (names of checks not yet complete).
// The check runs are pre-filtered by the GraphQL query to only include failures and pending runs.
func collectFindings(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	owner, repo, sha string,
	initialSuites gqlCheckSuitesConnection,
) (findings []callbacks.Finding, pendingChecks []string) {
	// Process failed check runs into findings
	processFailedRuns := func(runs []gqlCheckRunNode) {
		for _, run := range runs {
			findings = append(findings, callbacks.Finding{
				Kind:       callbacks.FindingKindCICheck,
				Identifier: fmt.Sprintf("%d", run.DatabaseId),
				Details:    formatCheckRunDetails(run.Name, run.Status, run.Conclusion, run.Title, run.Summary, run.Text, run.DetailsUrl),
				DetailsURL: run.DetailsUrl,
			})
		}
	}

	// Process pending check runs into pendingChecks list
	processPendingRuns := func(runs []gqlCheckRunNode) {
		for _, run := range runs {
			pendingChecks = append(pendingChecks, run.Name)
		}
	}

	// Track suites that need pagination for failed/pending runs
	type suitePagination struct {
		id     string
		cursor string
	}
	var failedRunsPagination, pendingRunsPagination []suitePagination

	for _, suite := range initialSuites.Nodes {
		processFailedRuns(suite.FailedRuns.Nodes)
		processPendingRuns(suite.PendingRuns.Nodes)

		if suite.FailedRuns.PageInfo.HasNextPage {
			failedRunsPagination = append(failedRunsPagination, suitePagination{
				id:     suite.Id,
				cursor: suite.FailedRuns.PageInfo.EndCursor,
			})
		}
		if suite.PendingRuns.PageInfo.HasNextPage {
			pendingRunsPagination = append(pendingRunsPagination, suitePagination{
				id:     suite.Id,
				cursor: suite.PendingRuns.PageInfo.EndCursor,
			})
		}
	}

	// Paginate through remaining failed runs within suites
	for _, sp := range failedRunsPagination {
		paginateFailedRuns(ctx, gqlClient, sp.id, sp.cursor, processFailedRuns)
	}

	// Paginate through remaining pending runs within suites
	for _, sp := range pendingRunsPagination {
		paginatePendingRuns(ctx, gqlClient, sp.id, sp.cursor, processPendingRuns)
	}

	// Paginate through remaining check suites if needed
	if initialSuites.PageInfo.HasNextPage {
		paginateCheckSuites(ctx, gqlClient, owner, repo, sha, initialSuites.PageInfo.EndCursor, processFailedRuns, processPendingRuns)
	}

	return findings, pendingChecks
}

// paginateFailedRuns fetches additional failed check runs for a suite.
func paginateFailedRuns(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	suiteID, cursor string,
	processRuns func([]gqlCheckRunNode),
) {
	for {
		var query struct {
			Node struct {
				CheckSuite struct {
					FailedRuns struct {
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
						Nodes []gqlCheckRunNode
					} `graphql:"failedRuns: checkRuns(first: 100, after: $cursor, filterBy: {conclusions: [FAILURE]})"`
				} `graphql:"... on CheckSuite"`
			} `graphql:"node(id: $suiteId)"`
		}

		if err := gqlClient.Query(ctx, "PaginateFailedRuns", &query, map[string]any{
			"suiteId": githubv4.ID(suiteID),
			"cursor":  githubv4.String(cursor),
		}); err != nil {
			clog.WarnContextf(ctx, "failed to paginate failed check runs: %v", err)
			return // Skip on error
		}

		processRuns(query.Node.CheckSuite.FailedRuns.Nodes)

		if !query.Node.CheckSuite.FailedRuns.PageInfo.HasNextPage {
			break
		}
		cursor = query.Node.CheckSuite.FailedRuns.PageInfo.EndCursor
	}
}

// paginatePendingRuns fetches additional pending check runs for a suite.
func paginatePendingRuns(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	suiteID, cursor string,
	processRuns func([]gqlCheckRunNode),
) {
	for {
		var query struct {
			Node struct {
				CheckSuite struct {
					PendingRuns struct {
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
						Nodes []gqlCheckRunNode
					} `graphql:"pendingRuns: checkRuns(first: 100, after: $cursor, filterBy: {statuses: [QUEUED, IN_PROGRESS, WAITING, PENDING, REQUESTED]})"`
				} `graphql:"... on CheckSuite"`
			} `graphql:"node(id: $suiteId)"`
		}

		if err := gqlClient.Query(ctx, "PaginatePendingRuns", &query, map[string]any{
			"suiteId": githubv4.ID(suiteID),
			"cursor":  githubv4.String(cursor),
		}); err != nil {
			clog.WarnContextf(ctx, "failed to paginate pending check runs: %v", err)
			return // Skip on error
		}

		processRuns(query.Node.CheckSuite.PendingRuns.Nodes)

		if !query.Node.CheckSuite.PendingRuns.PageInfo.HasNextPage {
			break
		}
		cursor = query.Node.CheckSuite.PendingRuns.PageInfo.EndCursor
	}
}

// paginateCheckSuites fetches additional check suites for a commit.
func paginateCheckSuites(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	owner, repo, sha, cursor string,
	processFailedRuns, processPendingRuns func([]gqlCheckRunNode),
) {
	for {
		var query struct {
			Repository struct {
				Object struct {
					Commit struct {
						CheckSuites struct {
							PageInfo struct {
								HasNextPage bool
								EndCursor   string
							}
							Nodes []struct {
								Id         string
								FailedRuns struct {
									PageInfo struct {
										HasNextPage bool
										EndCursor   string
									}
									Nodes []gqlCheckRunNode
								} `graphql:"failedRuns: checkRuns(first: 100, filterBy: {conclusions: [FAILURE]})"`
								PendingRuns struct {
									PageInfo struct {
										HasNextPage bool
										EndCursor   string
									}
									Nodes []gqlCheckRunNode
								} `graphql:"pendingRuns: checkRuns(first: 100, filterBy: {statuses: [QUEUED, IN_PROGRESS, WAITING, PENDING, REQUESTED]})"`
							}
						} `graphql:"checkSuites(first: 100, after: $cursor)"`
					} `graphql:"... on Commit"`
				} `graphql:"object(oid: $sha)"`
			} `graphql:"repository(owner: $owner, name: $repo)"`
		}

		if err := gqlClient.Query(ctx, "PaginateCheckSuites", &query, map[string]any{
			"owner":  githubv4.String(owner),
			"repo":   githubv4.String(repo),
			"sha":    githubv4.GitObjectID(sha),
			"cursor": githubv4.String(cursor),
		}); err != nil {
			return // Skip on error
		}

		for _, suite := range query.Repository.Object.Commit.CheckSuites.Nodes {
			processFailedRuns(suite.FailedRuns.Nodes)
			processPendingRuns(suite.PendingRuns.Nodes)

			// Handle nested check run pagination
			if suite.FailedRuns.PageInfo.HasNextPage {
				paginateFailedRuns(ctx, gqlClient, suite.Id, suite.FailedRuns.PageInfo.EndCursor, processFailedRuns)
			}
			if suite.PendingRuns.PageInfo.HasNextPage {
				paginatePendingRuns(ctx, gqlClient, suite.Id, suite.PendingRuns.PageInfo.EndCursor, processPendingRuns)
			}
		}

		if !query.Repository.Object.Commit.CheckSuites.PageInfo.HasNextPage {
			break
		}
		cursor = query.Repository.Object.Commit.CheckSuites.PageInfo.EndCursor
	}
}
