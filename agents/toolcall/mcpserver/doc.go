/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package mcpserver serves unified [toolcall.Tool] definitions as a Model
// Context Protocol server ([mcp.Server]) on any transport.
//
// [NewServer] lists a [toolcall.ToolProvider]'s tools once and converts
// them; [FromTools] converts a tool map directly. Each tool's parameters
// become its input schema, its annotations become MCP hints, and each call
// runs the tool's handler under its own [agenttrace] trace, with a response
// carrying an "error" key reported as an MCP tool error.
//
// A stdio server, as a CLI registers with an MCP client:
//
//	server, err := mcpserver.NewServer(ctx, "develop", provider, cb)
//	if err != nil {
//		return err
//	}
//	return server.Run(ctx, &mcp.StdioTransport{})
//
// The returned server is safe for concurrent use: the MCP SDK may run
// several tool calls at once, so tool handlers must be too.
package mcpserver
