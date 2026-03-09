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

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
)

const (
	defaultFindingReadLimit = 256_000
	maxFindingReadLimit     = 1_000_000
	maxFindingPatternLength = 512
	maxFindingSearchMatches = 1000
)

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

func (p findingToolsProvider[Resp, T]) Tools(cb FindingTools[T]) map[string]Tool[Resp] {
	tools := p.baseProvider.Tools(cb.base)
	maps.Copy(tools, findingToolDefs[Resp](cb.FindingCallbacks))
	return tools
}

func findingToolDefs[Resp any](cb callbacks.FindingCallbacks) map[string]Tool[Resp] {
	defs := make(map[string]Tool[Resp])

	if cb.HasGetDetails() {
		defs["get_finding_details"] = getFindingDetailsTool[Resp](cb.GetDetails)
	}
	if cb.HasGetLogs() {
		maps.Copy(defs, findingLogTools[Resp](cb.GetLogs))
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
				clog.FromContext(ctx).With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding details")
				result := params.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier})
				tc.Complete(result, err)
				return result
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

// findingLogTools returns search_finding_logs and read_finding_logs tools that share a log cache
// to avoid re-fetching the same logs on repeated calls.
func findingLogTools[Resp any](getLogs func(context.Context, callbacks.FindingKind, string) (string, error)) map[string]Tool[Resp] {
	type cacheKey struct{ kind, identifier string }
	cache := make(map[cacheKey]string)

	fetch := func(ctx context.Context, kind, identifier string) (string, error) {
		key := cacheKey{kind, identifier}
		if s, ok := cache[key]; ok {
			return s, nil
		}
		logs, err := getLogs(ctx, callbacks.FindingKind(kind), identifier)
		if err != nil {
			return "", err
		}
		cache[key] = logs
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
	seen := make(map[readCall]struct{})

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
			key := readCall{kind: kind, identifier: identifier, offset: offset, limit: limit}
			if _, dup := seen[key]; dup {
				clog.FromContext(ctx).With("kind", kind).With("identifier", identifier).With("offset", offset).Warn("Duplicate read_finding_logs call detected")
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
			seen[key] = struct{}{}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier, "offset": offset, "limit": limit})

			logs, err := fetch(ctx, kind, identifier)
			if err != nil {
				clog.FromContext(ctx).With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding logs")
				result := params.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier})
				tc.Complete(result, err)
				return result
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
	seen := make(map[searchCall]struct{})

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
			key := searchCall{kind: kind, identifier: identifier, pattern: pattern, skip: skip, limit: limit}
			if _, dup := seen[key]; dup {
				clog.FromContext(ctx).With("kind", kind).With("identifier", identifier).With("pattern", pattern).Warn("Duplicate search_finding_logs call detected")
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
			seen[key] = struct{}{}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"kind": kind, "identifier": identifier, "pattern": pattern, "skip": skip, "limit": limit})

			logs, err := fetch(ctx, kind, identifier)
			if err != nil {
				clog.FromContext(ctx).With("kind", kind).With("identifier", identifier).With("error", err).Error("Failed to get finding logs")
				result := params.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier})
				tc.Complete(result, err)
				return result
			}

			matches, totalMatches, err := findingSearchContent(logs, pattern, skip, limit)
			if err != nil {
				clog.FromContext(ctx).With("kind", kind).With("identifier", identifier).With("pattern", pattern).With("error", err).Error("Failed to search finding logs")
				result := params.ErrorWithContext(err, map[string]any{"kind": kind, "identifier": identifier, "pattern": pattern})
				tc.Complete(result, err)
				return result
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
	if offset < 0 {
		offset = 0
	}
	if offset > totalSize {
		offset = totalSize
	}
	if limit <= 0 {
		limit = defaultFindingReadLimit
	}
	if limit > maxFindingReadLimit {
		limit = maxFindingReadLimit
	}

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
