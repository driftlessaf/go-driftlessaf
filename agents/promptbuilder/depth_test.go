/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"strings"
	"testing"
)

func TestBindPromptDepthLimit(t *testing.T) {
	nest := func(depth int) *Prompt {
		p := MustNewPrompt("leaf")
		for range depth {
			p = MustNewPrompt("{{inner}}").MustBindPrompt("inner", p)
		}
		return p
	}

	if got, err := nest(maxCompositionDepth).Build(); err != nil {
		t.Errorf("Build() at the depth limit: unexpected error = %v", err)
	} else if got != "leaf" {
		t.Errorf("Build() at the depth limit = %q, want %q", got, "leaf")
	}

	if _, err := nest(maxCompositionDepth + 1).Build(); err == nil {
		t.Error("Build() beyond the depth limit expected error, got nil")
	} else if !strings.Contains(err.Error(), "composition exceeds") {
		t.Errorf("Build() error = %v, want composition depth error", err)
	}
}

func TestBindPromptSiblingDepth(t *testing.T) {
	// Sibling compositions each get the full depth budget: walking one
	// branch must not consume depth from the next.
	nest := func(depth int) *Prompt {
		p := MustNewPrompt("leaf")
		for range depth {
			p = MustNewPrompt("{{inner}}").MustBindPrompt("inner", p)
		}
		return p
	}

	p := MustNewPrompt("{{a}} {{b}}").
		MustBindPrompt("a", nest(maxCompositionDepth-1)).
		MustBindPrompt("b", nest(maxCompositionDepth-1))

	got, err := p.Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if want := "leaf leaf"; got != want {
		t.Errorf("Build() = %q, want %q", got, want)
	}
}

func TestBindPromptDiamondComposition(t *testing.T) {
	// Binding one prompt at two placeholders per level doubles the rendered
	// output each level: each distinct prompt builds once, and the size
	// limit catches the doubling before the allocation.
	diamond := func(levels int) *Prompt {
		p := MustNewPrompt("x")
		for range levels {
			p = MustNewPrompt("{{a}} {{b}}").
				MustBindPrompt("a", p).
				MustBindPrompt("b", p)
		}
		return p
	}

	got, err := diamond(10).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if want := strings.TrimRight(strings.Repeat("x ", 1<<10), " "); got != want {
		t.Errorf("Build() = %q, want %q", got, want)
	}

	if _, err := diamond(40).Build(); err == nil {
		t.Error("Build() expected size-limit error, got nil")
	} else if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("Build() error = %v, want size-limit error", err)
	}
}
