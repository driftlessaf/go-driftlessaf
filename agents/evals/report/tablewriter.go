/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report

import (
	"io"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/renderer"
	"github.com/olekukonko/tablewriter/tw"
)

// createStandardTable creates a table writer with standard formatting options
// This provides consistent table formatting across all evaluation reports
func createStandardTable(headers []string, w io.Writer) *tablewriter.Table {
	cfg := tablewriter.Config{
		Header: tw.CellConfig{
			Alignment:  tw.CellAlignment{Global: tw.AlignLeft},
			Formatting: tw.CellFormatting{AutoFormat: tw.Off},
		},
		Row: tw.CellConfig{
			Alignment: tw.CellAlignment{Global: tw.AlignLeft},
		},
		MaxWidth: 80,
		Behavior: tw.Behavior{TrimSpace: tw.Off},
	}
	return tablewriter.NewTable(w,
		tablewriter.WithConfig(cfg),
		tablewriter.WithHeader(headers),
		tablewriter.WithRenderer(renderer.NewBlueprint()),
		tablewriter.WithRendition(tw.Rendition{
			Symbols: tw.NewSymbols(tw.StyleMarkdown),
			Borders: tw.Border{
				Left:   tw.On,
				Top:    tw.Off,
				Right:  tw.On,
				Bottom: tw.Off,
			},
		}),
		tablewriter.WithRowAutoWrap(tw.WrapNone),
	)
}
