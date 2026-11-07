/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"embed"
	"fmt"
)

// NewPromptFromEmbed creates a prompt from the template file name in fsys.
// It is NewPrompt for a template too long to keep in a Go literal, such as a
// page of markdown.
//
// Like NewPrompt, it accepts only templates compiled into the program, never
// runtime data. The parameter is an embed.FS rather than an fs.FS for that
// reason: only a //go:embed directive can populate an embed.FS, while an
// fs.FS such as os.DirFS or fstest.MapFS can serve bytes read or built at run
// time.
func NewPromptFromEmbed(fsys embed.FS, name string) (*Prompt, error) {
	data, err := fsys.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("reading the embedded template %q: %w", name, err)
	}
	return newPrompt(string(data))
}

// MustNewPromptFromEmbed creates a prompt from the template file name in fsys
// and panics on error.
// This is syntactic sugar for Must(NewPromptFromEmbed(...))
func MustNewPromptFromEmbed(fsys embed.FS, name string) *Prompt {
	return Must(NewPromptFromEmbed(fsys, name))
}

// BindEmbed binds file, read from fsys, to the placeholder name verbatim. Unlike
// NewPromptFromEmbed, the file is not parsed as a template, so text that
// looks like a placeholder, such as melange's ${{package.version}}, renders
// as written. Use it to place trusted instructions kept in a file, such as a
// skill's SKILL.md, inside a prompt.
//
// The file is read once, when the binding is made, so every Build renders the
// same bytes. As with NewPromptFromEmbed, the embed.FS parameter limits the
// content to files compiled into the program.
func (p *Prompt) BindEmbed(name string, fsys embed.FS, file string) (*Prompt, error) {
	// Check the placeholder before the file so error precedence matches the
	// other Bind methods.
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	data, err := fsys.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading the embedded file %q: %w", file, err)
	}
	return p.bind(name, &literalBinding{val: string(data)})
}

// MustBindEmbed binds file, read from fsys, to the placeholder name verbatim,
// and panics on error.
// This is syntactic sugar for Must(p.BindEmbed(...))
func (p *Prompt) MustBindEmbed(name string, fsys embed.FS, file string) *Prompt {
	return Must(p.BindEmbed(name, fsys, file))
}
