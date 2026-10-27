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
