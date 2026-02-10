/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"errors"
	"fmt"
	"os"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"
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

	tools["read_file"] = claudetool.Metadata[Resp]{
		Definition: anthropic.ToolParam{
			Name:        "read_file",
			Description: anthropic.String("Read the complete content of a file from the codebase."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type: "object",
				Properties: map[string]any{
					"reasoning": map[string]any{
						"type":        "string",
						"description": "Explain why you are reading this file.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "The path to the file to read (relative to repository root)",
					},
				},
				Required: []string{"reasoning", "path"},
			},
		},
		Handler: claudeReadFileHandler[Resp](cb.ReadFile),
	}

	tools["write_file"] = claudetool.Metadata[Resp]{
		Definition: anthropic.ToolParam{
			Name:        "write_file",
			Description: anthropic.String("Create or update a file in the codebase."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type: "object",
				Properties: map[string]any{
					"reasoning": map[string]any{
						"type":        "string",
						"description": "Explain why you are writing this file.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "The path to the file to write (relative to repository root)",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "The complete content to write to the file",
					},
					"executable": map[string]any{
						"type":        "boolean",
						"description": "Whether the file should be executable (default: false)",
					},
				},
				Required: []string{"reasoning", "path", "content"},
			},
		},
		Handler: claudeWriteFileHandler[Resp](cb.WriteFile),
	}

	tools["delete_file"] = claudetool.Metadata[Resp]{
		Definition: anthropic.ToolParam{
			Name:        "delete_file",
			Description: anthropic.String("Delete a file from the codebase."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type: "object",
				Properties: map[string]any{
					"reasoning": map[string]any{
						"type":        "string",
						"description": "Explain why you are deleting this file.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "The path to the file to delete (relative to repository root)",
					},
				},
				Required: []string{"reasoning", "path"},
			},
		},
		Handler: claudeDeleteFileHandler[Resp](cb.DeleteFile),
	}

	tools["list_directory"] = claudetool.Metadata[Resp]{
		Definition: anthropic.ToolParam{
			Name:        "list_directory",
			Description: anthropic.String("List the contents of a directory."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type: "object",
				Properties: map[string]any{
					"reasoning": map[string]any{
						"type":        "string",
						"description": "Explain why you are listing this directory.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "The path to the directory to list (relative to repository root, use '.' for root)",
					},
				},
				Required: []string{"reasoning", "path"},
			},
		},
		Handler: claudeListDirectoryHandler[Resp](cb.ListDirectory),
	}

	tools["search_codebase"] = claudetool.Metadata[Resp]{
		Definition: anthropic.ToolParam{
			Name:        "search_codebase",
			Description: anthropic.String("Search for a pattern across all files in the codebase."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type: "object",
				Properties: map[string]any{
					"reasoning": map[string]any{
						"type":        "string",
						"description": "Explain what you are searching for and why.",
					},
					"pattern": map[string]any{
						"type":        "string",
						"description": "The regex pattern to search for",
					},
				},
				Required: []string{"reasoning", "pattern"},
			},
		},
		Handler: claudeSearchCodebaseHandler[Resp](cb.SearchCodebase),
	}

	return tools
}

func (p worktreeToolsProvider[Resp, T]) GoogleTools(cb WorktreeTools[T]) map[string]googletool.Metadata[Resp] {
	tools := p.baseProvider.GoogleTools(cb.base)

	tools["read_file"] = googletool.Metadata[Resp]{
		Definition: &genai.FunctionDeclaration{
			Name:        "read_file",
			Description: "Read the complete content of a file from the codebase.",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"reasoning": {Type: "string", Description: "Explain why you are reading this file."},
					"path":      {Type: "string", Description: "The path to the file to read (relative to repository root)"},
				},
				Required: []string{"reasoning", "path"},
			},
		},
		Handler: googleReadFileHandler[Resp](cb.ReadFile),
	}

	tools["write_file"] = googletool.Metadata[Resp]{
		Definition: &genai.FunctionDeclaration{
			Name:        "write_file",
			Description: "Create or update a file in the codebase.",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"reasoning":  {Type: "string", Description: "Explain why you are writing this file."},
					"path":       {Type: "string", Description: "The path to the file to write (relative to repository root)"},
					"content":    {Type: "string", Description: "The complete content to write to the file"},
					"executable": {Type: "boolean", Description: "Whether the file should be executable (default: false)"},
				},
				Required: []string{"reasoning", "path", "content"},
			},
		},
		Handler: googleWriteFileHandler[Resp](cb.WriteFile),
	}

	tools["delete_file"] = googletool.Metadata[Resp]{
		Definition: &genai.FunctionDeclaration{
			Name:        "delete_file",
			Description: "Delete a file from the codebase.",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"reasoning": {Type: "string", Description: "Explain why you are deleting this file."},
					"path":      {Type: "string", Description: "The path to the file to delete (relative to repository root)"},
				},
				Required: []string{"reasoning", "path"},
			},
		},
		Handler: googleDeleteFileHandler[Resp](cb.DeleteFile),
	}

	tools["list_directory"] = googletool.Metadata[Resp]{
		Definition: &genai.FunctionDeclaration{
			Name:        "list_directory",
			Description: "List the contents of a directory.",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"reasoning": {Type: "string", Description: "Explain why you are listing this directory."},
					"path":      {Type: "string", Description: "The path to the directory to list (relative to repository root, use '.' for root)"},
				},
				Required: []string{"reasoning", "path"},
			},
		},
		Handler: googleListDirectoryHandler[Resp](cb.ListDirectory),
	}

	tools["search_codebase"] = googletool.Metadata[Resp]{
		Definition: &genai.FunctionDeclaration{
			Name:        "search_codebase",
			Description: "Search for a pattern across all files in the codebase.",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"reasoning": {Type: "string", Description: "Explain what you are searching for and why."},
					"pattern":   {Type: "string", Description: "The regex pattern to search for"},
				},
				Required: []string{"reasoning", "pattern"},
			},
		},
		Handler: googleSearchCodebaseHandler[Resp](cb.SearchCodebase),
	}

	return tools
}

// Claude handlers

func claudeReadFileHandler[Resp any](readFile func(context.Context, string) (string, error)) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := claudetool.Param[string](params, "path")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing path parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"path": path})

		content, err := readFile(ctx, path)
		if err != nil {
			log.With("path", path).With("error", err).Error("Failed to read file")
			result := claudetool.ErrorWithContext(err, map[string]any{"path": path})
			tc.Complete(result, err)
			return result
		}

		result := map[string]any{"path": path, "content": content, "size": len(content)}
		tc.Complete(result, nil)
		return result
	}
}

func claudeWriteFileHandler[Resp any](writeFile func(context.Context, string, string, os.FileMode) error) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := claudetool.Param[string](params, "path")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing path parameter"))
			return errResp
		}

		content, errResp := claudetool.Param[string](params, "content")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing content parameter"))
			return errResp
		}

		executable, _ := claudetool.OptionalParam[bool](params, "executable", false)

		mode := os.FileMode(0644)
		if executable {
			mode = 0755
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"path": path, "size": len(content), "executable": executable})

		if err := writeFile(ctx, path, content, mode); err != nil {
			log.With("path", path).With("error", err).Error("Failed to write file")
			result := claudetool.ErrorWithContext(err, map[string]any{"path": path})
			tc.Complete(result, err)
			return result
		}

		result := map[string]any{"path": path, "written": len(content), "mode": fmt.Sprintf("%04o", mode)}
		tc.Complete(result, nil)
		return result
	}
}

func claudeDeleteFileHandler[Resp any](deleteFile func(context.Context, string) error) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := claudetool.Param[string](params, "path")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing path parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"path": path})

		if err := deleteFile(ctx, path); err != nil {
			log.With("path", path).With("error", err).Error("Failed to delete file")
			result := claudetool.ErrorWithContext(err, map[string]any{"path": path})
			tc.Complete(result, err)
			return result
		}

		result := map[string]any{"path": path, "deleted": true}
		tc.Complete(result, nil)
		return result
	}
}

func claudeListDirectoryHandler[Resp any](listDirectory func(context.Context, string) ([]string, error)) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := claudetool.Param[string](params, "path")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing path parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"path": path})

		entries, err := listDirectory(ctx, path)
		if err != nil {
			log.With("path", path).With("error", err).Error("Failed to list directory")
			result := claudetool.ErrorWithContext(err, map[string]any{"path": path})
			tc.Complete(result, err)
			return result
		}

		result := map[string]any{"path": path, "entries": entries, "count": len(entries)}
		tc.Complete(result, nil)
		return result
	}
}

func claudeSearchCodebaseHandler[Resp any](searchCodebase func(context.Context, string) ([]callbacks.Match, error)) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
		log := clog.FromContext(ctx)

		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return errResp
		}

		reasoning, errResp := claudetool.Param[string](params, "reasoning")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		pattern, errResp := claudetool.Param[string](params, "pattern")
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, params.RawInputs(), errors.New("missing pattern parameter"))
			return errResp
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, map[string]any{"pattern": pattern})

		matches, err := searchCodebase(ctx, pattern)
		if err != nil {
			log.With("pattern", pattern).With("error", err).Error("Failed to search codebase")
			result := claudetool.ErrorWithContext(err, map[string]any{"pattern": pattern})
			tc.Complete(result, err)
			return result
		}

		result := map[string]any{"pattern": pattern, "matches": matches, "count": len(matches)}
		tc.Complete(result, nil)
		return result
	}
}

// Google handlers

func googleReadFileHandler[Resp any](readFile func(context.Context, string) (string, error)) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := googletool.Param[string](call, "path")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing path parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path})

		content, err := readFile(ctx, path)
		if err != nil {
			log.With("path", path).With("error", err).Error("Failed to read file")
			result := googletool.ErrorWithContext(call, err, map[string]any{"path": path})
			tc.Complete(result.Response, err)
			return result
		}

		result := map[string]any{"path": path, "content": content, "size": len(content)}
		tc.Complete(result, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
	}
}

func googleWriteFileHandler[Resp any](writeFile func(context.Context, string, string, os.FileMode) error) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := googletool.Param[string](call, "path")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing path parameter"))
			return errResp
		}

		content, errResp := googletool.Param[string](call, "content")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing content parameter"))
			return errResp
		}

		executable, _ := googletool.OptionalParam(call, "executable", false)

		mode := os.FileMode(0644)
		if executable {
			mode = 0755
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "size": len(content), "executable": executable})

		if err := writeFile(ctx, path, content, mode); err != nil {
			log.With("path", path).With("error", err).Error("Failed to write file")
			result := googletool.ErrorWithContext(call, err, map[string]any{"path": path})
			tc.Complete(result.Response, err)
			return result
		}

		result := map[string]any{"path": path, "written": len(content), "mode": fmt.Sprintf("%04o", mode)}
		tc.Complete(result, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
	}
}

func googleDeleteFileHandler[Resp any](deleteFile func(context.Context, string) error) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := googletool.Param[string](call, "path")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing path parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path})

		if err := deleteFile(ctx, path); err != nil {
			log.With("path", path).With("error", err).Error("Failed to delete file")
			result := googletool.ErrorWithContext(call, err, map[string]any{"path": path})
			tc.Complete(result.Response, err)
			return result
		}

		result := map[string]any{"path": path, "deleted": true}
		tc.Complete(result, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
	}
}

func googleListDirectoryHandler[Resp any](listDirectory func(context.Context, string) ([]string, error)) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		path, errResp := googletool.Param[string](call, "path")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing path parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path})

		entries, err := listDirectory(ctx, path)
		if err != nil {
			log.With("path", path).With("error", err).Error("Failed to list directory")
			result := googletool.ErrorWithContext(call, err, map[string]any{"path": path})
			tc.Complete(result.Response, err)
			return result
		}

		result := map[string]any{"path": path, "entries": entries, "count": len(entries)}
		tc.Complete(result, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
	}
}

func googleSearchCodebaseHandler[Resp any](searchCodebase func(context.Context, string) ([]callbacks.Match, error)) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Resp], _ *Resp) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing reasoning parameter"))
			return errResp
		}
		log.With("reasoning", reasoning).Info("Tool call reasoning")

		pattern, errResp := googletool.Param[string](call, "pattern")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("missing pattern parameter"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"pattern": pattern})

		matches, err := searchCodebase(ctx, pattern)
		if err != nil {
			log.With("pattern", pattern).With("error", err).Error("Failed to search codebase")
			result := googletool.ErrorWithContext(call, err, map[string]any{"pattern": pattern})
			tc.Complete(result.Response, err)
			return result
		}

		result := map[string]any{"pattern": pattern, "matches": matches, "count": len(matches)}
		tc.Complete(result, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
	}
}
