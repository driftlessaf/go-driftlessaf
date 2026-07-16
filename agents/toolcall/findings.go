/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"sync"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/chainguard-dev/clog"
)

const (
	defaultFindingReadLimit = 256_000
	maxFindingReadLimit     = 1_000_000
	maxFindingPatternLength = 512
	maxFindingSearchMatches = 1000
)

// callGuard detects duplicate tool calls with identical arguments to prevent
// infinite loops. The mutex guards seen: a turn's tool calls may be
// dispatched concurrently.
type callGuard[K comparable] struct {
	mu   sync.Mutex
	seen map[K]struct{}
}

func newCallGuard[K comparable]() *callGuard[K] {
	return &callGuard[K]{seen: make(map[K]struct{})}
}

// duplicate records key as seen and reports whether it was already seen.
func (g *callGuard[K]) duplicate(key K) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, dup := g.seen[key]
	g.seen[key] = struct{}{}
	return dup
}

// FindingTools wraps a base tools type and adds finding callbacks.
type FindingTools[T any] struct {
	base T
	callbacks.FindingCallbacks
}

// NewFindingTools creates a FindingTools wrapping the given base tools.
func NewFindingTools[T any](base T, cb callbacks.FindingCallbacks) FindingTools[T] {
	return FindingTools[T]{base: base, FindingCallbacks: cb}
}

// findingToolsProvider wraps a base ToolProvider and adds finding tools.
type findingToolsProvider[Resp, T any] struct {
	baseProvider ToolProvider[Resp, T]
}

var _ ToolProvider[any, FindingTools[any]] = (*findingToolsProvider[any, any])(nil)

// NewFindingToolsProvider creates a provider that adds finding tools
// (get_finding_details, search_finding_logs, read_finding_logs) on top of the base provider's tools.
// The finding tools are only added if the corresponding callbacks are available.
func NewFindingToolsProvider[Resp, T any](base ToolProvider[Resp, T]) ToolProvider[Resp, FindingTools[T]] {
	return findingToolsProvider[Resp, T]{baseProvider: base}
}

func (p findingToolsProvider[Resp, T]) Tools(ctx context.Context, cb FindingTools[T]) (map[string]Tool[Resp], error) {
	tools, err := p.baseProvider.Tools(ctx, cb.base)
	if err != nil {
		return nil, err
	}
	maps.Copy(tools, findingToolDefs[Resp](cb.FindingCallbacks))
	return tools, nil
}

func findingToolDefs[Resp any](cb callbacks.FindingCallbacks) map[string]Tool[Resp] {
	defs := make(map[string]Tool[Resp])

	if cb.HasGetDetails() {
		defs["get_finding_details"] = getFindingDetailsTool[Resp](cb.GetDetails)
	}
	if cb.HasGetLogs() {
		maps.Copy(defs, findingLogTools[Resp](cb.GetLogs))
	}
	if cb.HasResolve() {
		defs["resolve_finding"] = resolveFindingTool[Resp](cb.Resolve)
	}
	if cb.HasRetry() {
		defs["retry_finding"] = retryFindingTool[Resp](cb.Retry)
	}

	return defs
}

func getFindingDetailsTool[Resp any](getDetails func(context.Context, callbacks.FindingKind, string) (string, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "get_finding_details",
			Description: "Get detailed information about a finding (CI failure, etc.) to understand what went wrong.",
			Parameters: []Parameter{
				{Name: "kind", Type: "string", Description: "The kind of finding (from the request's findings list)", Required: true},
				{Name: "identifier", Type: "string", Description: "The identifier of the finding (from the request's findings list)", Required: true},
			},
			Annotations: &ToolAnnotations{
				ReadOnly:    true,
				Destructive: Ptr(false),
				Idempotent:  true,
				// Finding details are scoped to the current PR's pre-loaded
				// state; the callback reads from an in-process cache rather
				// than making a live external network call at tool-call time.
				OpenWorld: Ptr(false),
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			kind, errResp := Param[string](call, trace, "kind")
			if errResp != nil {
				return errResp
			}

			identifier, errResp := Param[string](call, trace, "identifier")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier})

			details, err := getDetails(ctx, callbacks.FindingKind(kind), identifier)
			if err != nil {
				return completeError(ctx, tc, "Failed to get finding details", err, "kind", kind, "identifier", identifier)
			}

			result := map[string]any{
				"kind":       kind,
				"identifier": identifier,
				"details":    details,
			}
			tc.Complete(result, nil)
			return result
		},
	}
}

func resolveFindingTool[Resp any](resolve func(context.Context, string) error) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "resolve_finding",
			Description: "Resolve a finding after addressing the feedback. Only works for review thread findings, not CI checks or review bodies.",
			Parameters: []Parameter{{
				Name:        "identifier",
				Type:        "string",
				Description: "The identifier of the finding to resolve (from the request's findings list)",
				Required:    true,
			}},
			Annotations: &ToolAnnotations{
				Destructive: Ptr(false),
				// resolve_finding mutates PR review state, but the operation
				// is scoped to the current PR and does not open arbitrary
				// external connections; the callback uses a pre-authenticated
				// client bound to this PR's context.
				OpenWorld: Ptr(false),
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			identifier, errResp := Param[string](call, trace, "identifier")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"identifier": identifier})

			if err := resolve(ctx, identifier); err != nil {
				return completeError(ctx, tc, "Failed to resolve finding", err, "identifier", identifier)
			}

			result := map[string]any{
				"identifier": identifier,
				"resolved":   true,
			}
			tc.Complete(result, nil)
			return result
		},
	}
}

func retryFindingTool[Resp any](retry func(context.Context, callbacks.FindingKind, string) error) Tool[Resp] {
	type retryCall struct{ kind, identifier string }
	guard := newCallGuard[retryCall]()

	return Tool[Resp]{
		Def: Definition{
			Name: "retry_finding",
			Description: "Retry a failed finding (e.g., rerun a flaky CI check due to a network issue or transient infrastructure failure). " +
				"Only use this after reading the logs and confirming the failure is not caused by the code changes.",
			Parameters: []Parameter{
				{Name: "kind", Type: "string", Description: "The kind of finding (from the request's findings list)", Required: true},
				{Name: "identifier", Type: "string", Description: "The identifier of the finding (from the request's findings list)", Required: true},
			},
			Annotations: &ToolAnnotations{
				Destructive: Ptr(false),
				// retry_finding triggers a CI re-run scoped to the current
				// PR; it uses a pre-authenticated client and does not open
				// arbitrary external connections beyond the CI system already
				// associated with this PR's context.
				OpenWorld: Ptr(false),
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			kind, errResp := Param[string](call, trace, "kind")
			if errResp != nil {
				return errResp
			}

			identifier, errResp := Param[string](call, trace, "identifier")
			if errResp != nil {
				return errResp
			}

			// Detect duplicate calls to prevent infinite retry loops.
			if guard.duplicate(retryCall{kind: kind, identifier: identifier}) {
				clog.WarnContext(ctx, "Duplicate retry_finding call detected", "kind", kind, "identifier", identifier)
				tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier})
				resp := map[string]any{
					"error":      "duplicate call — this finding was already retried. Wait for the retry to complete instead of triggering another.",
					"kind":       kind,
					"identifier": identifier,
				}
				tc.Complete(resp, nil)
				return resp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier})

			if err := retry(ctx, callbacks.FindingKind(kind), identifier); err != nil {
				return completeError(ctx, tc, "Failed to retry finding", err, "kind", kind, "identifier", identifier)
			}

			result := map[string]any{
				"kind":       kind,
				"identifier": identifier,
				"retried":    true,
				"message":    "Finding retry triggered successfully.",
			}
			tc.Complete(result, nil)
			return result
		},
	}
}

// findingLogTools returns search_finding_logs and read_finding_logs tools that share a log cache
// to avoid re-fetching the same logs on repeated calls.
func findingLogTools[Resp any](getLogs func(context.Context, callbacks.FindingKind, string) (string, error)) map[string]Tool[Resp] {
	type cacheKey struct{ kind, identifier string }
	// mu guards cache: a turn's tool calls may be dispatched concurrently.
	// The lock is not held across getLogs, so concurrent misses on the same
	// key may fetch twice; last write wins.
	var mu sync.Mutex
	cache := make(map[cacheKey]string)

	fetch := func(ctx context.Context, kind, identifier string) (string, error) {
		key := cacheKey{kind, identifier}
		mu.Lock()
		s, ok := cache[key]
		mu.Unlock()
		if ok {
			return s, nil
		}
		logs, err := getLogs(ctx, callbacks.FindingKind(kind), identifier)
		if err != nil {
			return "", err
		}
		mu.Lock()
		cache[key] = logs
		mu.Unlock()
		return logs, nil
	}

	return map[string]Tool[Resp]{
		"read_finding_logs":   readFindingLogsTool[Resp](fetch),
		"search_finding_logs": searchFindingLogsTool[Resp](fetch),
	}
}

func readFindingLogsTool[Resp any](fetch func(context.Context, string, string) (string, error)) Tool[Resp] {
	type readCall struct {
		kind, identifier string
		offset           int64
		limit            int
	}
	guard := newCallGuard[readCall]()

	return Tool[Resp]{
		Def: Definition{
			Name: "read_finding_logs",
			Description: "Read log content for a finding starting at a byte offset. Returns content, next_offset to continue reading, and remaining bytes. " +
				"Use read_finding_logs(offset=0) to load initial content. Use search_finding_logs to find specific sections, then read_finding_logs to view context around matches.",
			Parameters: []Parameter{
				{Name: "kind", Type: "string", Description: "The kind of finding (from the request's findings list)", Required: true},
				{Name: "identifier", Type: "string", Description: "The identifier of the finding (from the request's findings list)", Required: true},
				{Name: "offset", Type: "integer", Description: "Byte offset to start reading from (default: 0)", Required: false},
				{Name: "limit", Type: "integer", Description: "Maximum bytes to read (default: 256000)", Required: false},
			},
			Annotations: &ToolAnnotations{
				ReadOnly:    true,
				Destructive: Ptr(false),
				Idempotent:  true,
				// Logs are fetched once from the CI system and cached
				// in-process; subsequent reads serve from the local cache
				// without additional external network calls.
				OpenWorld: Ptr(false),
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			kind, errResp := Param[string](call, trace, "kind")
			if errResp != nil {
				return errResp
			}
			identifier, errResp := Param[string](call, trace, "identifier")
			if errResp != nil {
				return errResp
			}
			offset, errResp := OptionalParam[int64](call, "offset", 0)
			if errResp != nil {
				return errResp
			}
			limit, errResp := OptionalParam[int](call, "limit", defaultFindingReadLimit)
			if errResp != nil {
				return errResp
			}

			// Detect duplicate calls to prevent infinite loops.
			if guard.duplicate(readCall{kind: kind, identifier: identifier, offset: offset, limit: limit}) {
				clog.WarnContext(ctx, "Duplicate read_finding_logs call detected", "kind", kind, "identifier", identifier, "offset", offset)
				tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier, "offset": offset, "limit": limit})
				resp := map[string]any{
					"error":      "duplicate call — this exact offset and limit was already read. Use the content from the previous call, or try a different offset.",
					"kind":       kind,
					"identifier": identifier,
					"offset":     offset,
					"limit":      limit,
				}
				tc.Complete(resp, nil)
				return resp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier, "offset": offset, "limit": limit})

			logs, err := fetch(ctx, kind, identifier)
			if err != nil {
				return completeError(ctx, tc, "Failed to get finding logs", err, "kind", kind, "identifier", identifier)
			}

			content, nextOffset, remaining := findingReadContent(logs, offset, limit)
			resp := map[string]any{
				"kind":       kind,
				"identifier": identifier,
				"content":    content,
				"remaining":  remaining,
				"total_size": int64(len(logs)),
			}
			if nextOffset != nil {
				resp["next_offset"] = *nextOffset
			}
			tc.Complete(resp, nil)
			return resp
		},
	}
}

func searchFindingLogsTool[Resp any](fetch func(context.Context, string, string) (string, error)) Tool[Resp] {
	type searchCall struct {
		kind, identifier, pattern string
		skip, limit               int
	}
	guard := newCallGuard[searchCall]()

	return Tool[Resp]{
		Def: Definition{
			Name: "search_finding_logs",
			Description: "Search log content for a finding using a regex pattern. Returns compact match pointers (byte offset, length) without content. " +
				"Use read_finding_logs with the returned offset to view matches in context, padding the offset and limit as needed for surrounding context.",
			Parameters: []Parameter{
				{Name: "kind", Type: "string", Description: "The kind of finding (from the request's findings list)", Required: true},
				{Name: "identifier", Type: "string", Description: "The identifier of the finding (from the request's findings list)", Required: true},
				{Name: "pattern", Type: "string", Description: "The regex pattern to search for", Required: true},
				{Name: "skip", Type: "integer", Description: "Number of matches to skip for pagination (default: 0)", Required: false},
				{Name: "limit", Type: "integer", Description: "Maximum matches to return (default: 20)", Required: false},
			},
			Annotations: &ToolAnnotations{
				ReadOnly:    true,
				Destructive: Ptr(false),
				Idempotent:  true,
				// Logs are fetched once from the CI system and cached
				// in-process; subsequent searches operate on the local cache
				// without additional external network calls.
				OpenWorld: Ptr(false),
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			kind, errResp := Param[string](call, trace, "kind")
			if errResp != nil {
				return errResp
			}
			identifier, errResp := Param[string](call, trace, "identifier")
			if errResp != nil {
				return errResp
			}
			pattern, errResp := Param[string](call, trace, "pattern")
			if errResp != nil {
				return errResp
			}
			skip, errResp := OptionalParam[int](call, "skip", 0)
			if errResp != nil {
				return errResp
			}
			limit, errResp := OptionalParam[int](call, "limit", 20)
			if errResp != nil {
				return errResp
			}

			// Detect duplicate calls to prevent infinite loops.
			if guard.duplicate(searchCall{kind: kind, identifier: identifier, pattern: pattern, skip: skip, limit: limit}) {
				clog.WarnContext(ctx, "Duplicate search_finding_logs call detected", "kind", kind, "identifier", identifier, "pattern", pattern)
				tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier, "pattern": pattern, "skip": skip, "limit": limit})
				resp := map[string]any{
					"error":      "duplicate call — this exact pattern, skip, and limit was already searched. Use the results from the previous call, or try a different pattern.",
					"kind":       kind,
					"identifier": identifier,
					"pattern":    pattern,
				}
				tc.Complete(resp, nil)
				return resp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier, "pattern": pattern, "skip": skip, "limit": limit})

			logs, err := fetch(ctx, kind, identifier)
			if err != nil {
				return completeError(ctx, tc, "Failed to get finding logs", err, "kind", kind, "identifier", identifier)
			}

			matches, totalMatches, err := findingSearchContent(logs, pattern, skip, limit)
			if err != nil {
				return completeError(ctx, tc, "Failed to search finding logs", err, "kind", kind, "identifier", identifier, "pattern", pattern)
			}

			resp := map[string]any{
				"kind":          kind,
				"identifier":    identifier,
				"pattern":       pattern,
				"matches":       matches,
				"total_matches": totalMatches,
				"has_more":      skip+len(matches) < totalMatches,
			}
			tc.Complete(resp, nil)
			return resp
		},
	}
}

// findingReadContent reads log content from a string with offset/limit pagination.
// Returns content, next_offset (nil at EOF), and remaining bytes.
func findingReadContent(s string, offset int64, limit int) (content string, nextOffset *int64, remaining int64) {
	totalSize := int64(len(s))
	offset = min(max(offset, 0), totalSize)
	// limit <= 0 falls back to the default rather than clamping to zero.
	if limit <= 0 {
		limit = defaultFindingReadLimit
	}
	limit = min(limit, maxFindingReadLimit)

	end := min(offset+int64(limit), totalSize)

	if end < totalSize {
		nextOffset = &end
		remaining = totalSize - end
	}
	return s[offset:end], nextOffset, remaining
}

// findingSearchContent searches log content for a regex pattern with skip/limit pagination.
// Returns matches (each with offset, length), total matches found (up to maxFindingSearchMatches), and any error.
func findingSearchContent(s string, pattern string, skip, limit int) ([]map[string]any, int, error) {
	if len(pattern) > maxFindingPatternLength {
		return nil, 0, fmt.Errorf("pattern too long (%d chars, max %d)", len(pattern), maxFindingPatternLength)
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid regex pattern: %w", err)
	}

	// Cap total matches to bound memory and CPU for pathological patterns.
	// totalFound is therefore an upper-bounded count: when totalFound == need,
	// there may be more matches beyond the cap. Callers should treat total_matches
	// as a lower bound in that case. This matches loganalyzer's behavior.
	need := min(skip+limit+1, maxFindingSearchMatches)
	indices := re.FindAllStringIndex(s, need)
	totalFound := len(indices)

	if skip >= totalFound {
		return []map[string]any{}, totalFound, nil
	}

	page := indices[skip:]
	if len(page) > limit {
		page = page[:limit]
	}

	matches := make([]map[string]any, 0, len(page))
	for _, idx := range page {
		matches = append(matches, map[string]any{
			"offset": int64(idx[0]),
			"length": idx[1] - idx[0],
		})
	}
	return matches, totalFound, nil
}
