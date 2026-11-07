/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder_test

import (
	"embed"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

//go:embed testdata/greeting.md testdata/invalid.md
var templates embed.FS

func TestNewPromptFromEmbed(t *testing.T) {
	p, err := promptbuilder.NewPromptFromEmbed(templates, "testdata/greeting.md")
	if err != nil {
		t.Fatalf("NewPromptFromEmbed: %v", err)
	}
	if _, ok := p.GetBindings()["name"]; !ok {
		t.Fatalf("bindings: got = %v, want = name", p.GetBindings())
	}
	got, err := p.MustBindStringLiteral("name", "world").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "Hello world.\n"; got != want {
		t.Errorf("Build: got = %q, want = %q", got, want)
	}
}

func TestNewPromptFromEmbedErrors(t *testing.T) {
	tests := []struct {
		name    string
		fsys    embed.FS
		file    string
		wantErr string
		wantIs  error
	}{{
		name:    "missing file",
		fsys:    templates,
		file:    "testdata/missing.md",
		wantErr: "testdata/missing.md",
		wantIs:  fs.ErrNotExist,
	}, {
		name:    "zero embed.FS",
		file:    "testdata/greeting.md",
		wantErr: "testdata/greeting.md",
	}, {
		name:    "invalid placeholder",
		fsys:    templates,
		file:    "testdata/invalid.md",
		wantErr: "test-case",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := promptbuilder.NewPromptFromEmbed(tc.fsys, tc.file)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NewPromptFromEmbed: got = %v, want an error containing %q", err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("NewPromptFromEmbed: got = %v, want an error wrapping %v", err, tc.wantIs)
			}
		})
	}
}

func TestMustNewPromptFromEmbedPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustNewPromptFromEmbed: got no panic, want one for a missing file")
		}
	}()
	promptbuilder.MustNewPromptFromEmbed(templates, "testdata/missing.md")
}

func TestBindEmbed(t *testing.T) {
	p := promptbuilder.MustNewPrompt("Before.\n{{file}}After.")
	bound, err := p.BindEmbed("file", templates, "testdata/invalid.md")
	if err != nil {
		t.Fatalf("BindEmbed: got = %v, want = nil", err)
	}
	got, err := bound.Build()
	if err != nil {
		t.Fatalf("Build: got = %v, want = nil", err)
	}
	// invalid.md holds {{test-case}}, which NewPromptFromEmbed rejects; a
	// verbatim binding renders it as written.
	if want := "Before.\nRun {{test-case}}.\nAfter."; got != want {
		t.Errorf("Build: got = %q, want = %q", got, want)
	}
	again, err := bound.Build()
	if err != nil {
		t.Fatalf("second Build: got = %v, want = nil", err)
	}
	if again != got {
		t.Errorf("second Build: got = %q, want = %q", again, got)
	}
	if _, err := p.Build(); err == nil {
		t.Error("original Build: got = nil, want an unbound-placeholder error")
	}
}

func TestBindEmbedErrors(t *testing.T) {
	tests := []struct {
		name    string
		prompt  *promptbuilder.Prompt
		fsys    embed.FS
		file    string
		wantErr string
		wantIs  error
	}{{
		name:    "missing file",
		prompt:  promptbuilder.MustNewPrompt("{{file}}"),
		fsys:    templates,
		file:    "testdata/missing.md",
		wantErr: "testdata/missing.md",
		wantIs:  fs.ErrNotExist,
	}, {
		name:    "zero embed.FS",
		prompt:  promptbuilder.MustNewPrompt("{{file}}"),
		fsys:    embed.FS{},
		file:    "testdata/greeting.md",
		wantErr: "testdata/greeting.md",
	}, {
		// The placeholder is checked before the file is read, as the other
		// Bind methods do, so the missing file goes unreported.
		name:    "missing placeholder",
		prompt:  promptbuilder.MustNewPrompt("{{other}}"),
		fsys:    templates,
		file:    "testdata/missing.md",
		wantErr: `binding "file" not found`,
	}, {
		name:    "already bound",
		prompt:  promptbuilder.MustNewPrompt("{{file}}").MustBindStringLiteral("file", "x"),
		fsys:    templates,
		file:    "testdata/greeting.md",
		wantErr: `binding "file" already bound`,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.prompt.BindEmbed("file", tc.fsys, tc.file)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("BindEmbed: got = %v, want an error containing %q", err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("BindEmbed: got = %v, want an error wrapping %v", err, tc.wantIs)
			}
		})
	}
}

func TestMustBindEmbedPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustBindEmbed: got no panic, want one for a missing file")
		}
	}()
	promptbuilder.MustNewPrompt("{{file}}").MustBindEmbed("file", templates, "testdata/missing.md")
}
