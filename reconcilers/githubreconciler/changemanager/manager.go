/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"text/template"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	internaltemplate "chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"
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

// WithDynamicCommitBudget measures the turn limit against commits since the last
// Session.ResetCommitBudget rather than the PR's total commit count. The baseline
// is persisted in the PR body, so resetting it grants a fresh WithMaxCommits-sized
// budget on the existing PR. Off by default.
func WithDynamicCommitBudget[T any]() Option[T] {
	return func(cm *CM[T]) {
		cm.dynamicCommitBudget = true
	}
}

// WithTraceDashboard sets the base URL of the agent-traces dashboard (e.g.
// "https://host/agent-traces/"). When set, the Trace-ID footer appended to PR
// bodies links the trace ID to the dashboard's trace view ("?trace=<id>") and,
// when the reconcile's agenttrace.ExecutionContext carries a reconciler key,
// adds a second link listing every agent run for this PR ("?reconcile=<key>").
// Query parameters already present on the base URL (e.g. "?env=staging") are
// preserved. Unset (default) keeps the plain-text Trace-ID footer.
func WithTraceDashboard[T any](baseURL string) Option[T] {
	return func(cm *CM[T]) {
		cm.traceDashboardURL = baseURL
	}
}

// metadata is changemanager state persisted in the PR body alongside the
// caller's data (see embeddedData).
type metadata struct {
	// CommitBudgetBaseline is the total commit count at the last
	// ResetCommitBudget call; see WithDynamicCommitBudget.
	CommitBudgetBaseline int `json:"commit_budget_baseline"`

	// ReasoningLog accumulates one agent-reasoning entry per commit the bot
	// created, oldest first; see Session.AppendReasoning. omitempty keeps
	// bodies embedded before the log existed parseable and byte-identical
	// when no entries exist.
	ReasoningLog []ReasoningEntry `json:"reasoning_log,omitempty"`
}

// ReasoningEntry is one iteration's agent-reasoning record, keyed by the
// headline of the commit that iteration produced. Entries are persisted in
// the PR body (see metadata) so per-commit reasoning survives body
// regenerations across iterations, letting callers render one reasoning
// block per commit rather than only the latest run's.
type ReasoningEntry struct {
	// CommitHeadline is the first line of the message of the commit this
	// reasoning explains.
	CommitHeadline string `json:"commit_headline"`
	// Summary is the truncated reasoning summary for the run that produced
	// the commit.
	Summary string `json:"summary"`
}

// embeddedData is the single JSON block changemanager stores in a PR body,
// wrapping the caller's data with changemanager's own metadata.
type embeddedData[T any] struct {
	Data T        `json:"data"`
	Meta metadata `json:"meta"`
}

// UnmarshalJSON also accepts the legacy block format, where the caller's data
// was embedded bare rather than wrapped — without this, bodies of pre-existing
// PRs would silently decode as zero-value Data.
func (e *embeddedData[T]) UnmarshalJSON(b []byte) error {
	var probe struct {
		Data json.RawMessage `json:"data"`
		Meta metadata        `json:"meta"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	if probe.Data == nil {
		return json.Unmarshal(b, &e.Data)
	}
	e.Meta = probe.Meta
	return json.Unmarshal(probe.Data, &e.Data)
}

// WithCloseOnEmptyDiff controls whether Upsert closes the PR when the branch
// has no net diff against base. Default true.
func WithCloseOnEmptyDiff[T any](close bool) Option[T] {
	return func(cm *CM[T]) {
		cm.closeOnEmptyDiff = close
	}
}

// WithManagedLabels declares the set of labels this reconciler owns. On an
// update, any managed label present on the PR but absent from the desired
// labels passed to Upsert is removed, while labels added by humans or other
// bots are preserved. Use this for labels the reconciler toggles on and off
// based on the current state (e.g. a "manual review needed" label applied
// only when a diff requires it). Labels not listed here are never removed.
func WithManagedLabels[T any](labels ...string) Option[T] {
	return func(cm *CM[T]) {
		cm.managedLabels = labels
	}
}

// CM manages the lifecycle of GitHub Pull Requests for a specific identity.
// It uses Go templates to generate PR titles and bodies from generic data of type T.
type CM[T any] struct {
	identity            string
	titleTemplate       *template.Template
	bodyTemplate        *template.Template
	templateExecutor    *internaltemplate.Template[embeddedData[T]]
	owner               string
	repo                string
	handlesFindings     bool
	maxCommits          int
	dynamicCommitBudget bool
	closeOnEmptyDiff    bool
	managedLabels       []string
	traceDashboardURL   string
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

// gqlStatusCheckRollupContext is one node of a commit's statusCheckRollup.contexts
// union connection. Only CheckRun contexts are consumed; StatusContext (legacy
// commit statuses) are ignored, matching the prior checkSuites-based behavior.
//
// The flat rollup replaces the old checkSuites(100) × checkRuns(100) nesting,
// which billed 3 GraphQL points; the rollup bills 1. Failed and pending runs are
// derived client-side from each run's conclusion/status (see collectFindings)
// rather than via the server-side filterBy the suite query used.
type gqlStatusCheckRollupContext struct {
	Typename string          `graphql:"__typename"`
	CheckRun gqlCheckRunNode `graphql:"... on CheckRun"`
}

type gqlRollupContextsConnection struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []gqlStatusCheckRollupContext
}

// pendingCheckStatuses is the set of CheckRun status values (uppercase GraphQL
// enums) that count as "not yet complete". Mirrors the prior pendingRuns filterBy.
var pendingCheckStatuses = map[string]struct{}{
	"QUEUED":      {},
	"IN_PROGRESS": {},
	"WAITING":     {},
	"PENDING":     {},
	"REQUESTED":   {},
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

	templateExecutor, err := internaltemplate.New[embeddedData[T]](identity, "-pr-data", "PR")
	if err != nil {
		return nil, fmt.Errorf("creating template executor: %w", err)
	}

	cm := &CM[T]{
		identity:         identity,
		titleTemplate:    titleTemplate,
		bodyTemplate:     bodyTemplate,
		templateExecutor: templateExecutor,
		closeOnEmptyDiff: true,
	}

	for _, opt := range opts {
		opt(cm)
	}

	return cm, nil
}

// Extract returns the embedded data from a PR body. Use when you have the
// body bytes (e.g. from go-github's PullRequests.Get) but no Session — for
// example, when an out-of-band trigger (PR webhook, CI status event) hands
// you a PR URL and you need to recover the originating reconciliation key.
// Additive helper: existing Session.Extract callers are unaffected.
func (cm *CM[T]) Extract(body string) (*T, error) {
	ed, err := cm.templateExecutor.Extract(body)
	if err != nil {
		return nil, err
	}
	return &ed.Data, nil
}

// render executes tmpl with the caller's *T. templateExecutor is typed on the
// embeddedData[T] wrapper, so it can't render the caller's templates directly.
func (cm *CM[T]) render(tmpl *template.Template, data *T) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

// SessionOption customizes a single NewSession call without affecting the
// CM's other sessions.
type SessionOption func(*sessionConfig)

type sessionConfig struct {
	branchPrefix string
}

// WithBranchPrefix overrides the CM identity as the head-branch prefix for
// this session only. Downstream systems that subscribe to PR events by
// head-branch prefix (e.g. an approver watching "doc-driven/") use this to
// route a subset of a bot's PRs without renaming every branch the bot
// manages. The prefix replaces the identity in branch construction only;
// the identity is still used for PR-body markers and templates.
//
// Note that an open PR created under a different prefix will not be found
// by a session using this one — the session queries PRs by head branch — so
// changing the prefix for an in-flight resource orphans its existing PR.
func WithBranchPrefix(prefix string) SessionOption {
	return func(sc *sessionConfig) {
		sc.branchPrefix = prefix
	}
}

// branchNameFor constructs the head-branch name and base ref for a resource:
// Path resources get {prefix}/{path-suffix} on the resource's ref, Issue
// resources get {prefix}/issue-{number} on main.
func branchNameFor(prefix string, res *githubreconciler.Resource) (branchName, ref string, err error) {
	switch res.Type {
	case githubreconciler.ResourceTypePath:
		return prefix + "/" + githubreconciler.PathToBranchSuffix(res.Path), res.Ref, nil
	case githubreconciler.ResourceTypeIssue:
		return prefix + "/issue-" + strconv.Itoa(res.Number), "main", nil // Issues don't have a ref, default to main
	default:
		return "", "", fmt.Errorf("change manager only supports Path and Issue resources, got: %v", res.Type)
	}
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
	opts ...SessionOption,
) (*Session[T], error) {
	sc := sessionConfig{branchPrefix: cm.identity}
	for _, opt := range opts {
		opt(&sc)
	}

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
	branchName, ref, err := branchNameFor(sc.branchPrefix, res)
	if err != nil {
		return nil, err
	}

	// Use GraphQL to fetch PR + check runs in a single query
	gqlClient := graphqlclient.NewGraphQLClient(client)

	var (
		prNumber      int
		prURL         string
		prBody        string
		prHeadSHA     string
		prMergeable   *bool
		prLabels      []string
		prAssignees   []string
		commitCount   int
		findings      []callbacks.Finding
		pendingChecks []string
		meta          metadata
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
								StatusCheckRollup struct {
									Contexts gqlRollupContextsConnection `graphql:"contexts(first: 100)"`
								} `graphql:"statusCheckRollup"`
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
		prHeadSHA = pr.HeadRefOid
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
			var err error
			findings, pendingChecks, err = collectFindings(ctx, gqlClient, owner, repo, pr.HeadRefOid, commit.StatusCheckRollup.Contexts)
			if err != nil {
				return nil, fmt.Errorf("collecting findings: %w", err)
			}
		}

		// Collect unresolved review thread findings from trusted authors
		findings = append(findings, collectThreadFindings(ctx, pr.ReviewThreads)...)

		// Collect review body findings from trusted authors on the current commit
		findings = append(findings, collectReviewBodyFindings(ctx, pr.HeadRefOid, pr.Reviews)...)

		// Recover the embedded metadata (e.g. the commit-budget baseline);
		// absent on PRs whose body predates this block.
		if ed, err := cm.templateExecutor.Extract(prBody); err == nil {
			meta = ed.Meta
		}
		// Clamp a stale baseline left behind when a rebase rebuilds the
		// branch, so it cannot grant budget beyond maxCommits.
		if meta.CommitBudgetBaseline > commitCount {
			meta.CommitBudgetBaseline = commitCount
		}
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
		prHeadSHA:     prHeadSHA,
		prMergeable:   prMergeable,
		prLabels:      prLabels,
		prAssignees:   prAssignees,
		commitCount:   commitCount,
		findings:      findings,
		pendingChecks: pendingChecks,
		meta:          meta,
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

		threadName := thread.Path
		if thread.Line > 0 {
			threadName = fmt.Sprintf("%s:%d", thread.Path, thread.Line)
		}
		findings = append(findings, callbacks.Finding{
			Kind:       callbacks.FindingKindReview,
			Identifier: thread.Id,
			Name:       threadName,
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
			Name:       "@" + review.Author.Login,
			Details:    formatReviewBodyDetails(review),
			DetailsURL: review.Url,
		})
	}

	return findings
}

// collectFindings extracts findings and pending checks from the head commit's
// statusCheckRollup contexts, handling pagination. Returns findings (failed
// checks) and pendingChecks (names of checks not yet complete). Failed and
// pending runs are classified client-side from each CheckRun's conclusion/status,
// since the flat rollup is not pre-filtered like the old per-suite checkRuns
// queries were.
func collectFindings(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	owner, repo, sha string,
	initialContexts gqlRollupContextsConnection,
) (findings []callbacks.Finding, pendingChecks []string, err error) {
	processContexts := func(nodes []gqlStatusCheckRollupContext) {
		for _, n := range nodes {
			// StatusContext (legacy commit statuses) and any other non-CheckRun
			// contexts are ignored, matching the prior checkSuites behavior.
			if n.Typename != "CheckRun" {
				continue
			}
			run := n.CheckRun
			_, pending := pendingCheckStatuses[run.Status]
			switch {
			case run.Conclusion == "FAILURE":
				findings = append(findings, callbacks.Finding{
					Kind:       callbacks.FindingKindCICheck,
					Identifier: fmt.Sprintf("%d", run.DatabaseId),
					Name:       run.Name,
					Details:    formatCheckRunDetails(run.Name, run.Status, run.Conclusion, run.Title, run.Summary, run.Text, run.DetailsUrl),
					DetailsURL: run.DetailsUrl,
				})
			case pending:
				pendingChecks = append(pendingChecks, run.Name)
			}
		}
	}

	processContexts(initialContexts.Nodes)

	// Paginate through remaining contexts if the head commit has >100 checks.
	// A pagination failure is fatal (see paginateRollupContexts): returning
	// truncated findings would let a red/pending PR read as green downstream.
	if initialContexts.PageInfo.HasNextPage {
		if err := paginateRollupContexts(ctx, gqlClient, owner, repo, sha, initialContexts.PageInfo.EndCursor, processContexts); err != nil {
			return nil, nil, err
		}
	}

	return findings, pendingChecks, nil
}

// paginateRollupContexts fetches additional statusCheckRollup contexts for a
// commit, when the head commit has more than 100 checks.
func paginateRollupContexts(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	owner, repo, sha, cursor string,
	process func([]gqlStatusCheckRollupContext),
) error {
	for {
		var query struct {
			Repository struct {
				Object struct {
					Commit struct {
						StatusCheckRollup struct {
							Contexts gqlRollupContextsConnection `graphql:"contexts(first: 100, after: $cursor)"`
						} `graphql:"statusCheckRollup"`
					} `graphql:"... on Commit"`
				} `graphql:"object(oid: $sha)"`
			} `graphql:"repository(owner: $owner, name: $repo)"`
		}

		// Do NOT swallow this error. A failed page (transient 502, rate-limit
		// throttle, etc.) would otherwise leave findings/pendingChecks truncated
		// to the pages fetched so far; if the failing runs live beyond page 1, a
		// red/pending PR reads as green downstream. Propagating the error fails
		// the reconcile so the workqueue retries — matching the prior shape, where
		// a failed initial query was always fatal.
		if err := gqlClient.Query(ctx, "PaginateRollupContexts", &query, map[string]any{
			"owner":  githubv4.String(owner),
			"repo":   githubv4.String(repo),
			"sha":    githubv4.GitObjectID(sha),
			"cursor": githubv4.String(cursor),
		}); err != nil {
			return fmt.Errorf("paginating status check rollup contexts: %w", err)
		}

		contexts := query.Repository.Object.Commit.StatusCheckRollup.Contexts
		process(contexts.Nodes)

		if !contexts.PageInfo.HasNextPage {
			break
		}
		cursor = contexts.PageInfo.EndCursor
	}
	return nil
}
