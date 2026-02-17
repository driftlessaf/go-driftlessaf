/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
)

// WorktreeTools wraps a base tools type and adds worktree callbacks.
type WorktreeTools[T any] struct {
	base T
	callbacks.WorktreeCallbacks
}

// NewWorktreeTools creates a WorktreeTools wrapping the given base tools.
func NewWorktreeTools[T any](base T, cb callbacks.WorktreeCallbacks) WorktreeTools[T] {
	return WorktreeTools[T]{base: base, WorktreeCallbacks: cb}
}

// worktreeToolsProvider wraps a base ToolProvider and adds worktree tools.
type worktreeToolsProvider[Resp, T any] struct {
	baseProvider ToolProvider[Resp, T]
}

var _ ToolProvider[any, WorktreeTools[any]] = (*worktreeToolsProvider[any, any])(nil)

// NewWorktreeToolsProvider creates a provider that adds worktree tools
// on top of the base provider's tools.
func NewWorktreeToolsProvider[Resp, T any](base ToolProvider[Resp, T]) ToolProvider[Resp, WorktreeTools[T]] {
	return worktreeToolsProvider[Resp, T]{baseProvider: base}
}

func (p worktreeToolsProvider[Resp, T]) ClaudeTools(cb WorktreeTools[T]) map[string]claudetool.Metadata[Resp] {
	tools := p.baseProvider.ClaudeTools(cb.base)
	for name, t := range worktreeToolDefs[Resp](cb.WorktreeCallbacks) {
		tools[name] = t.ClaudeMetadata()
	}
	return tools
}

func (p worktreeToolsProvider[Resp, T]) GoogleTools(cb WorktreeTools[T]) map[string]googletool.Metadata[Resp] {
	tools := p.baseProvider.GoogleTools(cb.base)
	for name, t := range worktreeToolDefs[Resp](cb.WorktreeCallbacks) {
		tools[name] = t.GoogleMetadata()
	}
	return tools
}

func worktreeToolDefs[Resp any](cb callbacks.WorktreeCallbacks) map[string]Tool[Resp] {
	return map[string]Tool[Resp]{
		"read_file":       readFileTool[Resp](cb.ReadFile),
		"edit_file":       editFileTool[Resp](cb.EditFile),
		"write_file":      writeFileTool[Resp](cb.WriteFile),
		"delete_file":     deleteFileTool[Resp](cb.DeleteFile),
		"move_file":       moveFileTool[Resp](cb.MoveFile),
		"copy_file":       copyFileTool[Resp](cb.CopyFile),
		"chmod":           chmodTool[Resp](cb.Chmod),
		"symlink":         symlinkTool[Resp](cb.CreateSymlink),
		"list_directory":  listDirectoryTool[Resp](cb.ListDirectory),
		"search_codebase": searchCodebaseTool[Resp](cb.SearchCodebase),
	}
}

func readFileTool[Resp any](readFile func(context.Context, string, int64, int) (callbacks.ReadResult, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "read_file",
			Description: "Read content from a file starting at a byte offset. Returns the content, next_offset to continue reading, and remaining bytes. Use list_directory to check file size before reading large files.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are reading this file.", Required: true},
				{Name: "path", Type: "string", Description: "The path to the file to read (relative to repository root)", Required: true},
				{Name: "offset", Type: "integer", Description: "Byte offset to start reading from (default: 0)", Required: false},
				{Name: "limit", Type: "integer", Description: "Maximum bytes to read (default: 20000). Pass -1 to read the entire file, but avoid this if you don't know the file size as it may be very large.", Required: false},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			offset, _ := OptionalParam[int64](call, "offset", 0)
			limit, _ := OptionalParam[int](call, "limit", 20000)

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "offset": offset, "limit": limit})

			result, err := readFile(ctx, path, offset, limit)
			if err != nil {
				log.With("path", path).With("error", err).Error("Failed to read file")
				resp := params.ErrorWithContext(err, map[string]any{"path": path})
				tc.Complete(resp, err)
				return resp
			}

			resp := map[string]any{
				"path":    path,
				"content": result.Content,
			}
			if result.NextOffset != nil {
				resp["next_offset"] = *result.NextOffset
			}
			resp["remaining"] = result.Remaining
			tc.Complete(resp, nil)
			return resp
		},
	}
}

func editFileTool[Resp any](editFile func(context.Context, string, string, string, bool) (callbacks.EditResult, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "edit_file",
			Description: "Edit a file by replacing exact text. The old_string must appear exactly once in the file unless replace_all is true. Use this instead of write_file for modifying existing files to avoid sending the entire file through context.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are making this edit.", Required: true},
				{Name: "path", Type: "string", Description: "The path to the file to edit (relative to repository root)", Required: true},
				{Name: "old_string", Type: "string", Description: "The exact text to find and replace. Maximum 32KB; use write_file for larger replacements.", Required: true},
				{Name: "new_string", Type: "string", Description: "The replacement text. Pass an empty string to delete the matched text. Maximum 32KB; use write_file for larger replacements.", Required: true},
				{Name: "replace_all", Type: "boolean", Description: "Replace all occurrences instead of requiring uniqueness (default: false)", Required: false},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			oldString, errResp := Param[string](call, trace, "old_string")
			if errResp != nil {
				return errResp
			}

			newString, errResp := Param[string](call, trace, "new_string")
			if errResp != nil {
				return errResp
			}

			replaceAll, _ := OptionalParam[bool](call, "replace_all", false)

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "replace_all": replaceAll})

			result, err := editFile(ctx, path, oldString, newString, replaceAll)
			if err != nil {
				log.With("path", path).With("error", err).Error("Failed to edit file")
				resp := params.ErrorWithContext(err, map[string]any{"path": path})
				tc.Complete(resp, err)
				return resp
			}

			resp := map[string]any{"path": path, "replacements": result.Replacements}
			tc.Complete(resp, nil)
			return resp
		},
	}
}

func writeFileTool[Resp any](writeFile func(context.Context, string, string, os.FileMode) error) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "write_file",
			Description: "Create or overwrite a file. For targeted modifications to existing files, prefer edit_file to avoid sending the entire file through context.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are writing this file.", Required: true},
				{Name: "path", Type: "string", Description: "The path to the file to write (relative to repository root)", Required: true},
				{Name: "content", Type: "string", Description: "The complete content to write to the file", Required: true},
				{Name: "executable", Type: "boolean", Description: "Whether the file should be executable (default: false)", Required: false},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			content, errResp := Param[string](call, trace, "content")
			if errResp != nil {
				return errResp
			}

			executable, _ := OptionalParam[bool](call, "executable", false)

			mode := os.FileMode(0644)
			if executable {
				mode = 0755
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "size": len(content), "executable": executable})

			if err := writeFile(ctx, path, content, mode); err != nil {
				log.With("path", path).With("error", err).Error("Failed to write file")
				result := params.ErrorWithContext(err, map[string]any{"path": path})
				tc.Complete(result, err)
				return result
			}

			result := map[string]any{"path": path, "written": len(content), "mode": fmt.Sprintf("%04o", mode)}
			tc.Complete(result, nil)
			return result
		},
	}
}

func deleteFileTool[Resp any](deleteFile func(context.Context, string) error) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "delete_file",
			Description: "Delete a file from the codebase.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are deleting this file.", Required: true},
				{Name: "path", Type: "string", Description: "The path to the file to delete (relative to repository root)", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path})

			if err := deleteFile(ctx, path); err != nil {
				log.With("path", path).With("error", err).Error("Failed to delete file")
				result := params.ErrorWithContext(err, map[string]any{"path": path})
				tc.Complete(result, err)
				return result
			}

			result := map[string]any{"path": path, "deleted": true}
			tc.Complete(result, nil)
			return result
		},
	}
}

func moveFileTool[Resp any](moveFile func(context.Context, string, string) error) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "move_file",
			Description: "Move or rename a file. No file content flows through context.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are moving this file.", Required: true},
				{Name: "path", Type: "string", Description: "The source path (relative to repository root)", Required: true},
				{Name: "destination", Type: "string", Description: "The destination path (relative to repository root)", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			destination, errResp := Param[string](call, trace, "destination")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"source": path, "destination": destination})

			if err := moveFile(ctx, path, destination); err != nil {
				log.With("source", path).With("destination", destination).With("error", err).Error("Failed to move file")
				result := params.ErrorWithContext(err, map[string]any{"source": path, "destination": destination})
				tc.Complete(result, err)
				return result
			}

			result := map[string]any{"source": path, "destination": destination}
			tc.Complete(result, nil)
			return result
		},
	}
}

func copyFileTool[Resp any](copyFile func(context.Context, string, string) error) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "copy_file",
			Description: "Copy a file. No file content flows through context.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are copying this file.", Required: true},
				{Name: "path", Type: "string", Description: "The source path (relative to repository root)", Required: true},
				{Name: "destination", Type: "string", Description: "The destination path (relative to repository root)", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			destination, errResp := Param[string](call, trace, "destination")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"source": path, "destination": destination})

			if err := copyFile(ctx, path, destination); err != nil {
				log.With("source", path).With("destination", destination).With("error", err).Error("Failed to copy file")
				result := params.ErrorWithContext(err, map[string]any{"source": path, "destination": destination})
				tc.Complete(result, err)
				return result
			}

			result := map[string]any{"source": path, "destination": destination}
			tc.Complete(result, nil)
			return result
		},
	}
}

func chmodTool[Resp any](chmod func(context.Context, string, os.FileMode) error) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "chmod",
			Description: "Change file permissions. Mode is an octal string like \"0755\" or \"0644\".",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are changing permissions.", Required: true},
				{Name: "path", Type: "string", Description: "The path to the file (relative to repository root)", Required: true},
				{Name: "mode", Type: "string", Description: "The file mode as an octal string (e.g., \"0755\", \"0644\")", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			modeStr, errResp := Param[string](call, trace, "mode")
			if errResp != nil {
				return errResp
			}

			mode, err := parseOctalMode(modeStr)
			if err != nil {
				trace.BadToolCall(call.ID, call.Name, call.Args, err)
				return params.Error("invalid mode %q: %v", modeStr, err)
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "mode": modeStr})

			if err := chmod(ctx, path, mode); err != nil {
				log.With("path", path).With("error", err).Error("Failed to chmod")
				result := params.ErrorWithContext(err, map[string]any{"path": path})
				tc.Complete(result, err)
				return result
			}

			result := map[string]any{"path": path, "mode": fmt.Sprintf("%04o", mode)}
			tc.Complete(result, nil)
			return result
		},
	}
}

func symlinkTool[Resp any](createSymlink func(context.Context, string, string) error) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "symlink",
			Description: "Create a symbolic link. The target should be a relative path.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are creating this symlink.", Required: true},
				{Name: "path", Type: "string", Description: "Where to create the symlink (relative to repository root)", Required: true},
				{Name: "target", Type: "string", Description: "What the symlink points to (relative path)", Required: true},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			target, errResp := Param[string](call, trace, "target")
			if errResp != nil {
				return errResp
			}

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "target": target})

			if err := createSymlink(ctx, path, target); err != nil {
				log.With("path", path).With("target", target).With("error", err).Error("Failed to create symlink")
				result := params.ErrorWithContext(err, map[string]any{"path": path, "target": target})
				tc.Complete(result, err)
				return result
			}

			result := map[string]any{"path": path, "target": target}
			tc.Complete(result, nil)
			return result
		},
	}
}

func listDirectoryTool[Resp any](listDirectory func(context.Context, string, string, int, int) (callbacks.ListResult, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "list_directory",
			Description: "List directory contents with ls -l style metadata (name, size, mode, type). Supports glob filtering with * wildcards or exact filename matching. Results are paginated.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain why you are listing this directory.", Required: true},
				{Name: "path", Type: "string", Description: "The path to the directory to list (relative to repository root, use '.' for root)", Required: true},
				{Name: "filter", Type: "string", Description: "Filter entries by glob pattern (e.g., \"*.go\") or exact filename (e.g., \"main.go\"). Only * wildcards are supported.", Required: false},
				{Name: "offset", Type: "integer", Description: "Number of entries to skip (default: 0)", Required: false},
				{Name: "limit", Type: "integer", Description: "Maximum entries to return (default: 50)", Required: false},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			path, errResp := Param[string](call, trace, "path")
			if errResp != nil {
				return errResp
			}

			filter, _ := OptionalParam[string](call, "filter", "")
			offset, _ := OptionalParam[int](call, "offset", 0)
			limit, _ := OptionalParam[int](call, "limit", 50)

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "filter": filter, "offset": offset, "limit": limit})

			result, err := listDirectory(ctx, path, filter, offset, limit)
			if err != nil {
				log.With("path", path).With("error", err).Error("Failed to list directory")
				resp := params.ErrorWithContext(err, map[string]any{"path": path})
				tc.Complete(resp, err)
				return resp
			}

			resp := formatListResult(path, result)
			tc.Complete(resp, nil)
			return resp
		},
	}
}

func searchCodebaseTool[Resp any](searchCodebase func(context.Context, string, string, string, int, int) (callbacks.SearchResult, error)) Tool[Resp] {
	return Tool[Resp]{
		Def: Definition{
			Name:        "search_codebase",
			Description: "Search for a regex pattern across files. Returns compact match pointers (path, byte offset, length) without content. Use read_file with the returned offset to view matches in context, padding the offset and limit as needed for surrounding context.",
			Parameters: []Parameter{
				{Name: "reasoning", Type: "string", Description: "Explain what you are searching for and why.", Required: true},
				{Name: "pattern", Type: "string", Description: "The regex pattern to search for", Required: true},
				{Name: "path", Type: "string", Description: "Directory to search within (relative to repository root, default: \".\")", Required: false},
				{Name: "filter", Type: "string", Description: "File filter — glob with * wildcards (e.g., \"*.go\") or exact filename (e.g., \"Makefile\")", Required: false},
				{Name: "offset", Type: "integer", Description: "Number of matches to skip (default: 0)", Required: false},
				{Name: "limit", Type: "integer", Description: "Maximum matches to return (default: 50)", Required: false},
			},
		},
		Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
			log := clog.FromContext(ctx)

			reasoning, errResp := Param[string](call, trace, "reasoning")
			if errResp != nil {
				return errResp
			}
			log.With("reasoning", reasoning).Info("Tool call reasoning")

			pattern, errResp := Param[string](call, trace, "pattern")
			if errResp != nil {
				return errResp
			}

			searchPath, _ := OptionalParam[string](call, "path", ".")
			filter, _ := OptionalParam[string](call, "filter", "")
			offset, _ := OptionalParam[int](call, "offset", 0)
			limit, _ := OptionalParam[int](call, "limit", 50)

			tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": searchPath, "pattern": pattern, "filter": filter, "offset": offset, "limit": limit})

			result, err := searchCodebase(ctx, searchPath, pattern, filter, offset, limit)
			if err != nil {
				log.With("pattern", pattern).With("error", err).Error("Failed to search codebase")
				resp := params.ErrorWithContext(err, map[string]any{"pattern": pattern})
				tc.Complete(resp, err)
				return resp
			}

			resp := formatSearchResult(searchPath, pattern, filter, result)
			tc.Complete(resp, nil)
			return resp
		},
	}
}

// Shared helpers

// formatListResult converts a ListResult into the JSON response map.
func formatListResult(path string, result callbacks.ListResult) map[string]any {
	entries := make([]map[string]any, 0, len(result.Entries))
	for _, e := range result.Entries {
		entry := map[string]any{
			"name": e.Name,
			"size": e.Size,
			"mode": fmt.Sprintf("%04o", e.Mode.Perm()),
			"type": e.Type,
		}
		if e.Target != "" {
			entry["target"] = e.Target
		}
		entries = append(entries, entry)
	}

	resp := map[string]any{
		"path":    path,
		"entries": entries,
		"count":   len(entries),
	}
	if result.NextOffset != nil {
		resp["next_offset"] = *result.NextOffset
	}
	resp["remaining"] = result.Remaining
	return resp
}

// formatSearchResult converts a SearchResult into the JSON response map.
func formatSearchResult(searchPath, pattern, filter string, result callbacks.SearchResult) map[string]any {
	matches := make([]map[string]any, 0, len(result.Matches))
	for _, m := range result.Matches {
		matches = append(matches, map[string]any{
			"path":   m.Path,
			"offset": m.Offset,
			"length": m.Length,
		})
	}

	resp := map[string]any{
		"path":    searchPath,
		"pattern": pattern,
		"matches": matches,
		"count":   len(matches),
	}
	if filter != "" {
		resp["filter"] = filter
	}
	if result.NextOffset != nil {
		resp["next_offset"] = *result.NextOffset
	}
	resp["has_more"] = result.HasMore
	return resp
}

// parseOctalMode parses an octal mode string like "0755" into os.FileMode.
func parseOctalMode(s string) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("parse octal mode: %w", err)
	}
	return os.FileMode(parsed), nil
}
