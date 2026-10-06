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
	"slices"
	"strconv"
	"strings"
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

// WithFindingReplies lets the fixer post a reply in a review thread, e.g. a
// short disposition before resolving one or a refutation when leaving one open.
// Off by default: the reply tool is registered only for a bot whose prompt
// directs it to use one, so a consumer that never mentions replies does not
// carry the capability. When enabled, reply bodies are sanitized before they
// reach GitHub (see sanitizeReplyBody): a body that begins with an @mention line
// carries a reviewer command, an embedded image or off-site link is untrusted
// content, and a secret-shaped string must never be echoed back onto a PR.
func WithFindingReplies[T any]() Option[T] {
	return func(cm *CM[T]) {
		cm.findingReplies = true
	}
}

// WithMaxCommits sets the commit budget allowed on a PR before the session
// reports StateMaxCommits. Each commit triggers a CI run, so this limits how
// many times the bot can iterate on a PR. A value of 0 (default) means no
// limit. WithMergeCommitsExcludedFromBudget can narrow which commits count.
func WithMaxCommits[T any](n int) Option[T] {
	return func(cm *CM[T]) {
		cm.maxCommits = n
	}
}

// WithMergeCommitsExcludedFromBudget makes merge commits count as zero toward
// WithMaxCommits. NewSession lists the pull request's commits when this option
// is enabled; reconcilers that do not opt in keep the existing query load and
// count every commit. It cannot be combined with WithDynamicCommitBudget
// because existing persisted baselines count all commits.
func WithMergeCommitsExcludedFromBudget[T any]() Option[T] {
	return func(cm *CM[T]) {
		cm.excludeMergeCommitsFromBudget = true
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

// defaultMaxBudgetResets bounds how many times Session.ResetCommitBudget renews a
// PR's commit budget when WithMaxBudgetResets is not set. It caps total commits
// at roughly (defaultMaxBudgetResets+1) * WithMaxCommits even when a review
// thread the bot cannot satisfy stays open.
const defaultMaxBudgetResets = 3

// WithMaxBudgetResets caps how many times Session.ResetCommitBudget renews a
// PR's commit budget (see WithDynamicCommitBudget). Once the cap is reached the
// budget stops renewing, so an unresolved review thread cannot extend the turn
// limit without bound. A value <= 0 keeps the default of 3. Operators typically
// wire this from an environment variable, alongside WithMaxCommits.
//
// The cap is enforced against an effective reset count that does not trust the
// PR body alone. The stored count is combined with a lower bound the commit
// history implies: with WithMaxCommits set to N, each budget admits at most N
// commits, so a PR with C commits has already consumed at least
// floor((C-1)/N) resets. The larger of the two governs, so lowering the stored
// count in the body can never unlock more than one budget beyond what the
// commits already prove, and adding commits only tightens the bound.
func WithMaxBudgetResets[T any](n int) Option[T] {
	return func(cm *CM[T]) {
		cm.maxBudgetResets = n
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

	// BudgetResetCount counts how many times ResetCommitBudget has renewed the
	// commit budget on this PR. ResetCommitBudget stops renewing once it reaches
	// the configured cap (WithMaxBudgetResets), bounding total commits even when
	// a review thread stays open. omitempty keeps older bodies parseable and
	// byte-identical when no reset has happened.
	BudgetResetCount int `json:"budget_reset_count,omitempty"`

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

// WithTrustedReviewAuthors extends the trusted set with GitHub login names
// whose review comments and review bodies are consumed regardless of author
// association. A comment or review is trusted when its association is one of
// OWNER, MEMBER, COLLABORATOR OR its author is a bot named in this allowlist.
// Use it for GitHub App reviewers whose association is NONE, e.g.
// "some-reviewer[bot]".
//
// The allowlist matches only a bot actor: a GitHub App (__typename "Bot") or a
// login carrying the reserved "[bot]" suffix. A human User account is never
// matched against the allowlist, even when its login equals an entry, so a user
// named like an allowlisted App cannot borrow its trust. A bot may be named in
// either form, "some-reviewer" or "some-reviewer[bot]", and matches whether the
// author arrives suffixed (the REST shape) or bare with __typename "Bot" (the
// GraphQL shape GitHub returns for an App). The default (empty allowlist) keeps
// association-only filtering. Repeated calls add to the set.
func WithTrustedReviewAuthors[T any](logins ...string) Option[T] {
	return func(cm *CM[T]) {
		if len(logins) == 0 {
			return
		}
		if cm.trustedReviewAuthors == nil {
			cm.trustedReviewAuthors = make(map[string]struct{}, len(logins))
		}
		for _, l := range logins {
			if l != "" {
				cm.trustedReviewAuthors[l] = struct{}{}
			}
		}
	}
}

// WithManagedLabels declares the set of labels this reconciler owns. On an
// update, any managed label present on the PR but absent from the desired
// labels passed to Upsert is removed, while labels added by humans or other
// bots are preserved. Use this for labels the reconciler toggles on and off
// based on the current state (e.g. a "manual review needed" label applied
// only when a diff requires it). Labels not listed here are never removed.
// Labels exceeding GitHub's limit use the same normalized name as Upsert.
// Repeated calls add to the set.
func WithManagedLabels[T any](labels ...string) Option[T] {
	return func(cm *CM[T]) {
		for _, l := range normalizeLabels(labels) {
			if !slices.Contains(cm.managedLabels, l) {
				cm.managedLabels = append(cm.managedLabels, l)
			}
		}
	}
}

// WithIgnoredChecks names check runs the reconciler cannot act on. A run with
// one of these names never becomes a finding or a pending check, whatever its
// status or conclusion, so it cannot trigger an iteration or hold the PR as
// pending. Use it for checks that gate something other than CI, such as an
// auto-merge eligibility verdict, or that fail for reasons no commit can fix.
// Repeated calls add to the set.
//
// Runs match by name alone, and any GitHub App can create a run with a listed
// name. Ignoring a run only stops the reconciler acting on it, so name a check
// here only if nothing else relies on the reconciler to act on that check.
func WithIgnoredChecks[T any](names ...string) Option[T] {
	return func(cm *CM[T]) {
		if cm.ignoredChecks == nil {
			cm.ignoredChecks = make(map[string]struct{}, len(names))
		}
		for _, n := range names {
			cm.ignoredChecks[n] = struct{}{}
		}
	}
}

// WithCheckSuites reads check runs through the head commit's check suites
// rather than its statusCheckRollup. The rollup also carries commit statuses, so
// reading it requires the statuses permission on top of checks, though only
// check runs are consumed; with this option the token needs checks but not
// statuses. Enable it once the reconciler's GitHub App has checks: read, since
// without it every session fails rather than seeing no checks.
//
// appIDs, when given, limits the suites read to those created by the named
// GitHub Apps (e.g. 15368 for GitHub Actions), so another app's checks cannot
// trigger an iteration or hold the PR as pending. Each app beyond the first
// costs one more GraphQL request per session, and the session query bills 2
// GraphQL points rather than the rollup's 1 regardless of appIDs, because
// checkSuites(100) x checkRuns(100) is priced on page size.
func WithCheckSuites[T any](appIDs ...int64) Option[T] {
	return func(cm *CM[T]) {
		cm.checkSuites = true
		cm.checkApps = nil
		for _, id := range appIDs {
			if !slices.Contains(cm.checkApps, id) {
				cm.checkApps = append(cm.checkApps, id)
			}
		}
	}
}

// CM manages the lifecycle of GitHub Pull Requests for a specific identity.
// It uses Go templates to generate PR titles and bodies from generic data of type T.
type CM[T any] struct {
	identity                      string
	titleTemplate                 *template.Template
	bodyTemplate                  *template.Template
	templateExecutor              *internaltemplate.Template[embeddedData[T]]
	owner                         string
	repo                          string
	handlesFindings               bool
	findingReplies                bool
	maxCommits                    int
	excludeMergeCommitsFromBudget bool
	dynamicCommitBudget           bool
	maxBudgetResets               int
	closeOnEmptyDiff              bool
	managedLabels                 []string
	traceDashboardURL             string
	// trustedReviewAuthors are GitHub login names trusted regardless of
	// author association; see WithTrustedReviewAuthors.
	trustedReviewAuthors map[string]struct{}
	// checkProviders handle CI check findings, the first match winning; see
	// WithCheckProviders.
	checkProviders []CheckProvider
	// ignoredChecks are check run names excluded from findings and pending
	// checks; see WithIgnoredChecks.
	ignoredChecks map[string]struct{}
	// checkSuites reads checks through check suites rather than the rollup,
	// limited to the suites of checkApps when non-empty; see WithCheckSuites.
	checkSuites bool
	checkApps   []int64
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

// gqlCheckSuiteNode is one check suite of the head commit with its latest
// check runs; superseded attempts of a rerun check are excluded so they cannot
// resurface as findings.
type gqlCheckSuiteNode struct {
	Id        string
	CheckRuns gqlCheckRunsConnection `graphql:"checkRuns(first: 100, filterBy: {checkType: LATEST})"`
}

// gqlCheckSuitesConnection is a commit's checkSuites connection; see
// WithCheckSuites.
type gqlCheckSuitesConnection struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []gqlCheckSuiteNode
}

// gqlStatusCheckRollupContext is one node of a commit's statusCheckRollup.contexts
// union connection. Only CheckRun contexts are consumed; StatusContext (legacy
// commit statuses) are ignored.
//
// The flat rollup bills 1 GraphQL point where checkSuites(100) x checkRuns(100)
// bills 2, but requires the statuses permission; see WithCheckSuites.
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
// enums) that count as "not yet complete".
var pendingCheckStatuses = map[string]struct{}{
	"QUEUED":      {},
	"IN_PROGRESS": {},
	"WAITING":     {},
	"PENDING":     {},
	"REQUESTED":   {},
}

// GraphQL types for querying review threads
type gqlThreadComment struct {
	Author            gqlActor
	AuthorAssociation string
	Body              string
	Url               string
	Commit            struct{ Oid string }
	CreatedAt         string
	// ViewerDidAuthor reports whether the authenticated token (this bot's
	// installation) wrote the comment, so a thread the bot answered can be told
	// apart from one still awaiting it without knowing the bot's login string.
	ViewerDidAuthor bool `graphql:"viewerDidAuthor"`
}

// gqlActor selects a comment or review author's login and its GraphQL type.
// GitHub reports a GitHub App author two ways: the REST API appends a "[bot]"
// suffix to the login, while GraphQL drops the suffix and reports the type as
// "Bot" instead (Typename). Both are needed to match an App reviewer against
// an allowlist regardless of which API surfaced the author.
type gqlActor struct {
	Login    string
	Typename string `graphql:"__typename"`
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
	Author            gqlActor
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

// botLoginSuffix is the "[bot]" login suffix GitHub appends to a GitHub App's
// login in REST responses. GraphQL drops the suffix and reports the actor's
// __typename as "Bot" instead. Normalizing across the two shapes lets an
// allowlist entry match an App reviewer regardless of which API surfaced it.
const botLoginSuffix = "[bot]"

// authorTrusted reports whether a comment or review is trusted. Trust comes
// from one of two sources: the author's association is in
// trustedAuthorAssociations, or the author is a bot named in trustedAuthors (see
// WithTrustedReviewAuthors). typename is the author's GraphQL __typename ("Bot"
// for a GitHub App).
//
// The allowlist matches only a bot actor: a GitHub App (typename "Bot") or a
// login carrying the reserved "[bot]" suffix. A human User account is never
// matched against the allowlist, even when its login equals an allowlist entry,
// so a user named like an allowlisted App cannot borrow its trust; human trust
// comes only from association. An allowlist entry may name a bot in either form,
// "some-reviewer" or "some-reviewer[bot]", and matches whether the author
// arrives bare with typename "Bot" (GraphQL shape) or suffixed (REST shape). A
// nil or empty trustedAuthors leaves association-only filtering.
func authorTrusted(association, login, typename string, trustedAuthors map[string]struct{}) bool {
	if _, ok := trustedAuthorAssociations[association]; ok {
		return true
	}
	// Beyond association, only a bot actor can be trusted via the allowlist.
	// GitHub reserves the "[bot]" suffix to Apps, so either the GraphQL type or
	// the suffix identifies a bot; a human User matches neither.
	if typename != "Bot" && !strings.HasSuffix(login, botLoginSuffix) {
		return false
	}
	// Match either author shape against either allowlist form: bare "reviewer"
	// or suffixed "reviewer[bot]".
	if _, ok := trustedAuthors[login]; ok {
		return true
	}
	// A GitHub App author arrives bare with typename "Bot" from GraphQL; the
	// allowlist may carry the suffixed "[bot]" form.
	if typename == "Bot" {
		if _, ok := trustedAuthors[login+botLoginSuffix]; ok {
			return true
		}
	}
	// A suffixed author (REST shape) may match a bare allowlist entry.
	if base, ok := strings.CutSuffix(login, botLoginSuffix); ok {
		if _, ok := trustedAuthors[base]; ok {
			return true
		}
	}
	return false
}

// displayReviewAuthor renders an author's login for model-facing finding
// details. A GitHub App author arrives bare with typename "Bot" from GraphQL,
// but the fixer prompt recognizes an automated reviewer by the "[bot]" suffix,
// so the suffix is restored here. A login already carrying the suffix, or a
// non-Bot author, is returned unchanged.
func displayReviewAuthor(login, typename string) string {
	if typename == "Bot" && !strings.HasSuffix(login, botLoginSuffix) {
		return login + botLoginSuffix
	}
	return login
}

// logUntrustedSkip records a dropped comment or review from an untrusted
// author. When the operator configured an allowlist, the drop is logged at Info
// so the operator can see which authors were filtered; otherwise it stays at
// Debug.
func logUntrustedSkip(ctx context.Context, trustedAuthors map[string]struct{}, format string, args ...any) {
	if len(trustedAuthors) > 0 {
		clog.InfoContextf(ctx, format, args...)
		return
	}
	clog.DebugContextf(ctx, format, args...)
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
	if cm.excludeMergeCommitsFromBudget && cm.dynamicCommitBudget {
		return nil, errors.New("WithMergeCommitsExcludedFromBudget cannot be combined with WithDynamicCommitBudget")
	}
	// After the options, so configured providers are tried first.
	cm.checkProviders = append(cm.checkProviders, githubActions{})

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
	skipChecks   bool
}

// WithoutGlobalStatus omits global CI status fetching for callers that do not use them.
// This is useful for integrations that do not need or want to see data from other applications.
// PR metadata and review findings remain available.
// CI failures do not trigger WithFindingsIteration; HasPendingChecks returns false
// without establishing that CI passed.
func WithoutGlobalStatus() SessionOption {
	return func(sc *sessionConfig) { sc.skipChecks = true }
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
// request, with pagination for repos with many checks. Check runs are read
// through the head commit's statusCheckRollup, which needs the checks and
// statuses permissions, unless WithCheckSuites is set.
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
		prDraft       bool
		prLabels      []string
		prAssignees   []string
		commitCount   int
		budgetCommits int
		findings      []callbacks.Finding
		pendingChecks []string
		meta          metadata

		awaitingReviewThreads map[string]struct{}
	)

	vars := map[string]any{
		"owner":         githubv4.String(owner),
		"repo":          githubv4.String(repo),
		"headRef":       githubv4.String(branchName),
		"baseRef":       githubv4.String(ref),
		"includeChecks": githubv4.Boolean(!sc.skipChecks),
	}
	var pr *prInfo
	if cm.checkSuites {
		// The first app's suites ride along with the PR query; collect fetches
		// any further apps' suites separately.
		vars["checkSuiteFilter"] = checkSuiteFilter(cm.checkApps, 0)
		pr, err = queryPRInfo[suiteChecks](ctx, gqlClient, vars)
	} else {
		pr, err = queryPRInfo[rollupChecks](ctx, gqlClient, vars)
	}
	if err != nil {
		return nil, err
	}

	// Process the PR if one exists
	if pr != nil {
		prNumber = pr.Number
		prURL = pr.Url
		prBody = pr.Body
		prHeadSHA = pr.HeadRefOid
		prDraft = pr.IsDraft
		// Map GraphQL mergeable status to bool pointer
		switch pr.Mergeable {
		case "MERGEABLE":
			prMergeable = new(true)
		case "CONFLICTING":
			prMergeable = new(false)
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

		commitCount = pr.commitCount
		budgetCommits = commitCount
		if cm.excludeMergeCommitsFromBudget && cm.maxCommits > 0 {
			budgetCommits, err = countNonMergeCommits(ctx, gqlClient, owner, repo, prNumber)
			if err != nil {
				return nil, fmt.Errorf("counting non-merge commits: %w", err)
			}
		}

		// Collect all check runs, handling pagination
		if !sc.skipChecks && pr.checks != nil {
			var err error
			findings, pendingChecks, err = pr.checks.collect(ctx, gqlClient, owner, repo, pr.HeadRefOid, cm.checkApps, cm.ignoredChecks)
			if err != nil {
				return nil, fmt.Errorf("collecting findings: %w", err)
			}
		}

		// Collect unresolved review thread findings from trusted authors
		threadFindings, awaiting := collectThreadFindings(ctx, pr.ReviewThreads, cm.trustedReviewAuthors)
		findings = append(findings, threadFindings...)
		awaitingReviewThreads = awaiting

		// Collect review body findings from trusted authors on the current commit
		findings = append(findings, collectReviewBodyFindings(ctx, pr.HeadRefOid, pr.Reviews, cm.trustedReviewAuthors)...)

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
		prDraft:       prDraft,
		prLabels:      prLabels,
		prAssignees:   prAssignees,
		commitCount:   commitCount,
		budgetCommits: budgetCommits,
		findings:      findings,
		pendingChecks: pendingChecks,
		meta:          meta,

		reviewThreadsAwaitingReply: awaitingReviewThreads,
		threads:                    &threadActions{},
	}, nil
}

// collectThreadFindings extracts findings from unresolved review threads.
// A thread is included regardless of which commit it was left on, but only
// while it still awaits this bot: its most recent relevant comment is a trusted
// author's, not the bot's own reply (see threadAwaitsReply). A thread the bot
// answered last is settled and produces no finding until a trusted author
// replies again, so a refuted finding the bot replied to and left open does not
// resurface every reconcile as an identical finding, which drove one duplicate
// reply per cycle. Only comments from trusted authors are included; threads with
// no trusted comments are skipped. trustedAuthors names logins trusted
// regardless of association (see authorTrusted).
// It also returns the set of thread node ids the returned findings cover, all of
// which await this bot. HasUnresolvedReviews consults it so a thread the bot has
// already answered does not keep renewing the commit budget.
func collectThreadFindings(ctx context.Context, threads gqlReviewThreadsConnection, trustedAuthors map[string]struct{}) ([]callbacks.Finding, map[string]struct{}) {
	findings := make([]callbacks.Finding, 0, len(threads.Nodes))
	awaiting := make(map[string]struct{})

	for _, thread := range threads.Nodes {
		if thread.IsResolved {
			clog.DebugContextf(ctx, "Skipping resolved review thread id=%s path=%s", thread.Id, thread.Path)
			continue
		}

		// Filter to comments from trusted authors only
		var trustedComments []gqlThreadComment
		for _, c := range thread.Comments.Nodes {
			if authorTrusted(c.AuthorAssociation, c.Author.Login, c.Author.Typename, trustedAuthors) {
				trustedComments = append(trustedComments, c)
			} else {
				logUntrustedSkip(ctx, trustedAuthors, "Skipping untrusted thread comment author=%s association=%s thread=%s", displayReviewAuthor(c.Author.Login, c.Author.Typename), c.AuthorAssociation, thread.Id)
			}
		}
		if len(trustedComments) == 0 {
			clog.DebugContextf(ctx, "Skipping review thread with no trusted comments id=%s path=%s", thread.Id, thread.Path)
			continue
		}

		// A settled thread (the bot's own reply is its latest relevant comment)
		// is excluded so the fixer does not see, and reply to, a finding it has
		// already answered. A trusted reply after the bot's reopens it.
		if !threadAwaitsReply(thread.Comments.Nodes, trustedAuthors) {
			clog.DebugContextf(ctx, "Skipping settled review thread (bot replied last) id=%s path=%s", thread.Id, thread.Path)
			continue
		}

		// The finding name is model-facing text outside any wrapper, so the
		// contributor-controlled path is sanitized before it becomes the name.
		threadName := sanitizeInlinePath(thread.Path)
		if thread.Line > 0 {
			threadName = fmt.Sprintf("%s:%d", threadName, thread.Line)
		}
		findings = append(findings, callbacks.Finding{
			Kind:       callbacks.FindingKindReview,
			Identifier: thread.Id,
			Name:       threadName,
			Details:    formatThreadDetails(thread.Path, thread.Line, thread.IsOutdated, trustedComments),
			DetailsURL: trustedComments[0].Url,
		})
		awaiting[thread.Id] = struct{}{}
	}

	return findings, awaiting
}

// threadAwaitsReply reports whether an unresolved review thread is still waiting
// on this bot. It walks the thread's comments in chronological order and tracks
// the last one that is relevant: either the bot's own comment (ViewerDidAuthor)
// or a comment from a trusted author. The thread awaits the bot when that last
// relevant comment is not the bot's own. A thread the bot answered last is
// settled until a trusted author replies again, so a refuted finding the bot
// replied to and left open no longer renews the commit budget. Untrusted
// comments neither settle nor reopen a thread.
func threadAwaitsReply(comments []gqlThreadComment, trustedAuthors map[string]struct{}) bool {
	awaiting := false
	for _, c := range comments {
		switch {
		case c.ViewerDidAuthor:
			awaiting = false
		case authorTrusted(c.AuthorAssociation, c.Author.Login, c.Author.Typename, trustedAuthors):
			awaiting = true
		}
	}
	return awaiting
}

// reviewBodyIdentifierPrefix distinguishes review body findings from thread findings.
const reviewBodyIdentifierPrefix = "review-body:"

// collectReviewBodyFindings extracts findings from non-empty review bodies by trusted
// authors on the current commit. Review bodies lack a resolution concept, so they are
// filtered by commit association: once the bot pushes a new commit, old bodies drop out.
// trustedAuthors names logins trusted regardless of association (see authorTrusted).
func collectReviewBodyFindings(ctx context.Context, headRefOid string, reviews gqlReviewBodiesConnection, trustedAuthors map[string]struct{}) []callbacks.Finding {
	var findings []callbacks.Finding

	for _, review := range reviews.Nodes {
		// An empty body carries nothing to act on regardless of who wrote it.
		// GitHub records one for every reply posted through the REST API, so
		// the bot's own thread replies arrive here as empty reviews under its
		// login; checking the body first keeps them out of the trust log.
		if review.Body == "" {
			clog.DebugContextf(ctx, "Skipping review body with empty body author=%s", review.Author.Login)
			continue
		}
		if !authorTrusted(review.AuthorAssociation, review.Author.Login, review.Author.Typename, trustedAuthors) {
			logUntrustedSkip(ctx, trustedAuthors, "Skipping untrusted review body author=%s association=%s", displayReviewAuthor(review.Author.Login, review.Author.Typename), review.AuthorAssociation)
			continue
		}
		if review.Commit.Oid != headRefOid {
			clog.DebugContextf(ctx, "Skipping review body on stale commit author=%s commit=%s head=%s", review.Author.Login, review.Commit.Oid, headRefOid)
			continue
		}

		findings = append(findings, callbacks.Finding{
			Kind:       callbacks.FindingKindReview,
			Identifier: reviewBodyIdentifierPrefix + fmt.Sprintf("%d", review.DatabaseId),
			Name:       "@" + displayReviewAuthor(review.Author.Login, review.Author.Typename),
			Details:    formatReviewBodyDetails(review),
			DetailsURL: review.Url,
		})
	}

	return findings
}

// prInfoFields are the GetPRInfo pull request fields other than its head
// commit's checks.
type prInfoFields struct {
	Number     int
	Url        string
	Body       string
	Mergeable  string // MERGEABLE, CONFLICTING, UNKNOWN
	IsDraft    bool
	HeadRefOid string
	Labels     struct {
		Nodes []struct {
			Name string
		}
	} `graphql:"labels(first: 100)"`
	Assignees struct {
		Nodes []struct {
			Login string
		}
	} `graphql:"assignees(first: 100)"`
	ReviewThreads gqlReviewThreadsConnection `graphql:"reviewThreads(first: 100)"`
	Reviews       gqlReviewBodiesConnection  `graphql:"reviews(first: 100)"`
}

// prInfo is the open pull request found by GetPRInfo.
type prInfo struct {
	prInfoFields
	commitCount int
	// checks is the first page of the head commit's checks, nil when the PR
	// has no commits.
	checks headChecks
}

// headChecks selects a head commit's check runs in the GetPRInfo query, and
// collects findings from them, paginating past the first page.
type headChecks interface {
	collect(ctx context.Context, gqlClient *graphqlclient.GraphQLClient, owner, repo, sha string, apps []int64, ignored map[string]struct{}) ([]callbacks.Finding, []string, error)
}

// rollupChecks reads checks through statusCheckRollup (the default).
type rollupChecks struct {
	StatusCheckRollup struct {
		Contexts gqlRollupContextsConnection `graphql:"contexts(first: 100)"`
	} `graphql:"statusCheckRollup @include(if: $includeChecks)"`
}

func (r rollupChecks) collect(ctx context.Context, gqlClient *graphqlclient.GraphQLClient, owner, repo, sha string, _ []int64, ignored map[string]struct{}) ([]callbacks.Finding, []string, error) {
	return collectFindings(ctx, gqlClient, owner, repo, sha, r.StatusCheckRollup.Contexts, ignored)
}

// suiteChecks reads checks through check suites; see WithCheckSuites.
type suiteChecks struct {
	CheckSuites gqlCheckSuitesConnection `graphql:"checkSuites(first: 100, filterBy: $checkSuiteFilter) @include(if: $includeChecks)"`
}

func (s suiteChecks) collect(ctx context.Context, gqlClient *graphqlclient.GraphQLClient, owner, repo, sha string, apps []int64, ignored map[string]struct{}) ([]callbacks.Finding, []string, error) {
	return collectSuiteFindings(ctx, gqlClient, owner, repo, sha, apps, s.CheckSuites, ignored)
}

// queryPRInfo runs GetPRInfo, selecting the head commit's checks with C. It
// returns nil when no open pull request matches.
func queryPRInfo[C headChecks](ctx context.Context, gqlClient *graphqlclient.GraphQLClient, vars map[string]any) (*prInfo, error) {
	var query struct {
		Repository struct {
			PullRequests struct {
				Nodes []struct {
					prInfoFields
					Commits struct {
						TotalCount int
						Nodes      []struct {
							Commit C
						}
					} `graphql:"commits(last: 1)"`
				}
			} `graphql:"pullRequests(headRefName: $headRef, baseRefName: $baseRef, states: [OPEN], first: 1)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	if err := gqlClient.Query(ctx, "GetPRInfo", &query, vars); err != nil {
		return nil, fmt.Errorf("querying pull request: %w", err)
	}
	if len(query.Repository.PullRequests.Nodes) == 0 {
		return nil, nil
	}
	node := query.Repository.PullRequests.Nodes[0]
	pr := &prInfo{prInfoFields: node.prInfoFields, commitCount: node.Commits.TotalCount}
	if len(node.Commits.Nodes) > 0 {
		pr.checks = node.Commits.Nodes[0].Commit
	}
	return pr, nil
}

// checkSuiteFilter returns the checkSuites filter selecting the i-th of apps,
// or nil, which selects every app's suites, when apps is empty.
func checkSuiteFilter(apps []int64, i int) *githubv4.CheckSuiteFilter {
	if len(apps) == 0 {
		return nil
	}
	return &githubv4.CheckSuiteFilter{AppID: githubv4.NewInt(githubv4.Int(apps[i]))}
}

// collectFindings extracts findings and pending checks from the head commit's
// statusCheckRollup contexts, handling pagination. Returns findings (failed
// checks) and pendingChecks (names of checks not yet complete). Runs named in
// ignored are skipped entirely.
//
// A pagination failure is fatal: returning truncated findings would let a
// red/pending PR read as green downstream.
func collectFindings(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	owner, repo, sha string,
	initialContexts gqlRollupContextsConnection,
	ignored map[string]struct{},
) ([]callbacks.Finding, []string, error) {
	c := &checkCollector{gqlClient: gqlClient, owner: owner, repo: repo, sha: sha, ignored: ignored}
	if err := c.addContexts(ctx, initialContexts); err != nil {
		return nil, nil, err
	}
	return c.findings, c.pendingChecks, nil
}

// collectSuiteFindings is collectFindings for check suites (see
// WithCheckSuites), handling pagination of suites and of runs within a suite.
// initialSuites holds the suites of the first of apps (or of every app, when
// apps is empty); the suites of the remaining apps are fetched here.
func collectSuiteFindings(
	ctx context.Context,
	gqlClient *graphqlclient.GraphQLClient,
	owner, repo, sha string,
	apps []int64,
	initialSuites gqlCheckSuitesConnection,
	ignored map[string]struct{},
) ([]callbacks.Finding, []string, error) {
	c := &checkCollector{gqlClient: gqlClient, owner: owner, repo: repo, sha: sha, ignored: ignored}
	if err := c.addSuites(ctx, checkSuiteFilter(apps, 0), initialSuites); err != nil {
		return nil, nil, err
	}
	for i := 1; i < len(apps); i++ {
		if err := c.fetchSuites(ctx, checkSuiteFilter(apps, i), nil); err != nil {
			return nil, nil, err
		}
	}
	return c.findings, c.pendingChecks, nil
}

// checkCollector accumulates findings and pending checks across the pages of a
// commit's check suites and their check runs.
type checkCollector struct {
	gqlClient        *graphqlclient.GraphQLClient
	owner, repo, sha string
	ignored          map[string]struct{}

	findings      []callbacks.Finding
	pendingChecks []string
}

// addRuns classifies check runs: a FAILURE conclusion becomes a finding and a
// not-yet-complete status a pending check. Any other conclusion (including
// CANCELLED and TIMED_OUT) is ignored.
func (c *checkCollector) addRuns(runs []gqlCheckRunNode) {
	for _, run := range runs {
		if _, skip := c.ignored[run.Name]; skip {
			continue
		}
		_, pending := pendingCheckStatuses[run.Status]
		switch {
		case run.Conclusion == "FAILURE":
			c.findings = append(c.findings, callbacks.Finding{
				Kind:       callbacks.FindingKindCICheck,
				Identifier: fmt.Sprintf("%d", run.DatabaseId),
				Name:       run.Name,
				Details:    formatCheckRunDetails(run.Name, run.Status, run.Conclusion, run.Title, run.Summary, run.Text, run.DetailsUrl),
				DetailsURL: run.DetailsUrl,
			})
		case pending:
			c.pendingChecks = append(c.pendingChecks, run.Name)
		}
	}
}

// addContexts processes one page of statusCheckRollup contexts, then fetches
// the remaining pages. StatusContext (legacy commit statuses) and any other
// non-CheckRun contexts are ignored.
func (c *checkCollector) addContexts(ctx context.Context, contexts gqlRollupContextsConnection) error {
	for {
		for _, n := range contexts.Nodes {
			if n.Typename == "CheckRun" {
				c.addRuns([]gqlCheckRunNode{n.CheckRun})
			}
		}
		if !contexts.PageInfo.HasNextPage {
			return nil
		}

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
		if err := c.gqlClient.Query(ctx, "PaginateRollupContexts", &query, map[string]any{
			"owner":  githubv4.String(c.owner),
			"repo":   githubv4.String(c.repo),
			"sha":    githubv4.GitObjectID(c.sha),
			"cursor": githubv4.String(contexts.PageInfo.EndCursor),
		}); err != nil {
			return fmt.Errorf("paginating status check rollup contexts: %w", err)
		}
		contexts = query.Repository.Object.Commit.StatusCheckRollup.Contexts
	}
}

// addSuites processes one page of check suites selected by filter, then
// fetches the remaining runs of any suite and the remaining pages of suites.
func (c *checkCollector) addSuites(ctx context.Context, filter *githubv4.CheckSuiteFilter, suites gqlCheckSuitesConnection) error {
	for _, suite := range suites.Nodes {
		c.addRuns(suite.CheckRuns.Nodes)
		if suite.CheckRuns.PageInfo.HasNextPage {
			if err := c.fetchRuns(ctx, suite.Id, suite.CheckRuns.PageInfo.EndCursor); err != nil {
				return err
			}
		}
	}
	if suites.PageInfo.HasNextPage {
		return c.fetchSuites(ctx, filter, githubv4.NewString(githubv4.String(suites.PageInfo.EndCursor)))
	}
	return nil
}

// fetchSuites fetches the commit's check suites selected by filter, starting
// after cursor (from the first suite when cursor is nil).
func (c *checkCollector) fetchSuites(ctx context.Context, filter *githubv4.CheckSuiteFilter, cursor *githubv4.String) error {
	var query struct {
		Repository struct {
			Object struct {
				Commit struct {
					CheckSuites gqlCheckSuitesConnection `graphql:"checkSuites(first: 100, after: $cursor, filterBy: $checkSuiteFilter)"`
				} `graphql:"... on Commit"`
			} `graphql:"object(oid: $sha)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	if err := c.gqlClient.Query(ctx, "PaginateCheckSuites", &query, map[string]any{
		"owner":            githubv4.String(c.owner),
		"repo":             githubv4.String(c.repo),
		"sha":              githubv4.GitObjectID(c.sha),
		"cursor":           cursor,
		"checkSuiteFilter": filter,
	}); err != nil {
		return fmt.Errorf("paginating check suites: %w", err)
	}
	return c.addSuites(ctx, filter, query.Repository.Object.Commit.CheckSuites)
}

// fetchRuns fetches the remaining check runs of a suite with more than 100.
func (c *checkCollector) fetchRuns(ctx context.Context, suiteID, cursor string) error {
	for {
		var query struct {
			Node struct {
				CheckSuite struct {
					CheckRuns gqlCheckRunsConnection `graphql:"checkRuns(first: 100, after: $cursor, filterBy: {checkType: LATEST})"`
				} `graphql:"... on CheckSuite"`
			} `graphql:"node(id: $suiteId)"`
		}
		if err := c.gqlClient.Query(ctx, "PaginateCheckRuns", &query, map[string]any{
			"suiteId": githubv4.ID(suiteID),
			"cursor":  githubv4.String(cursor),
		}); err != nil {
			return fmt.Errorf("paginating check runs: %w", err)
		}

		runs := query.Node.CheckSuite.CheckRuns
		c.addRuns(runs.Nodes)
		if !runs.PageInfo.HasNextPage {
			return nil
		}
		cursor = runs.PageInfo.EndCursor
	}
}
