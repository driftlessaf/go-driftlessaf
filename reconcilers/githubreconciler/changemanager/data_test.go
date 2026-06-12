/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"strings"
	"testing"
	"text/template"
)

type testData struct {
	PackageName string
	Version     string
	Commit      string
	// Nested exercises Extract on bodies whose JSON contains nested objects.
	Nested *testNested `json:",omitempty"`
}

type testNested struct {
	Field1 string
	Field2 int
}

func TestNew(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}/{{.Version}}"))
	bodyTmpl := template.Must(template.New("body").Parse("Update {{.PackageName}} to {{.Version}}"))

	tests := []struct {
		name          string
		identity      string
		titleTemplate *template.Template
		bodyTemplate  *template.Template
		wantErr       bool
		errContains   string
	}{{
		name:          "valid templates",
		identity:      "test-bot",
		titleTemplate: titleTmpl,
		bodyTemplate:  bodyTmpl,
		wantErr:       false,
	}, {
		name:          "nil title template",
		identity:      "test-bot",
		titleTemplate: nil,
		bodyTemplate:  bodyTmpl,
		wantErr:       true,
		errContains:   "titleTemplate cannot be nil",
	}, {
		name:          "nil body template",
		identity:      "test-bot",
		titleTemplate: titleTmpl,
		bodyTemplate:  nil,
		wantErr:       true,
		errContains:   "bodyTemplate cannot be nil",
	}, {
		name:          "both templates nil",
		identity:      "test-bot",
		titleTemplate: nil,
		bodyTemplate:  nil,
		wantErr:       true,
		errContains:   "titleTemplate cannot be nil",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm, err := New[testData](tt.identity, tt.titleTemplate, tt.bodyTemplate)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error: got = %v, wantErr = %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				if err == nil {
					t.Error("New() error: got = nil, want = non-nil error")
				} else if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("New() error message: got = %q, want to contain %q", err.Error(), tt.errContains)
				}
			} else {
				if cm == nil {
					t.Fatal("New() result: got = nil, want = non-nil CM")
					return
				}
				if cm.identity != tt.identity {
					t.Errorf("New() identity: got = %q, want = %q", cm.identity, tt.identity)
				}
			}
		})
	}
}

// TestExtractFromBody verifies the body-only Extract helper round-trips data
// embedded with the same template, including JSON with nested objects (the
// shape that motivated this helper — see linearreconciler/metareconciler).
func TestExtractFromBody(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("x"))
	bodyTmpl := template.Must(template.New("body").Parse("x"))
	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := &testData{
		PackageName: "pkg",
		Version:     "1.2.3",
		Commit:      "abcdef",
		Nested:      &testNested{Field1: "hello", Field2: 42},
	}
	body, err := cm.templateExecutor.Embed("body text", &embeddedData[testData]{Data: *want})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	got, err := cm.Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.PackageName != want.PackageName || got.Version != want.Version || got.Commit != want.Commit {
		t.Errorf("Extract scalar fields = %+v, want %+v", got, want)
	}
	if got.Nested == nil || got.Nested.Field1 != want.Nested.Field1 || got.Nested.Field2 != want.Nested.Field2 {
		t.Errorf("Extract nested = %+v, want %+v", got.Nested, want.Nested)
	}

	if _, err := cm.Extract("PR body with no embedded data"); err == nil {
		t.Error("Extract on body without data: got nil error, want non-nil")
	}
}

// TestExtractLegacyFormat verifies Extract still recovers data from PR bodies
// created before the embeddedData wrapper, which embed the caller's data bare.
func TestExtractLegacyFormat(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("x"))
	bodyTmpl := template.Must(template.New("body").Parse("x"))
	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := `PR body text

<!--test-bot-pr-data-->
<!--
{
  "PackageName": "pkg",
  "Version": "1.2.3",
  "Commit": "abcdef"
}
-->
<!--/test-bot-pr-data-->`

	got, err := cm.Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.PackageName != "pkg" || got.Version != "1.2.3" || got.Commit != "abcdef" {
		t.Errorf("Extract = %+v, want pkg/1.2.3/abcdef", got)
	}
}

// TestExtractFromBody_RealisticPRBody mirrors the surface-area of a real PR
// body produced by a downstream consumer (markdown link, em-dash, code
// fences) to catch regressions where Extract becomes sensitive to the body
// content surrounding the embedded data block.
func TestExtractFromBody_RealisticPRBody(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}"))
	bodyTmpl := template.Must(template.New("body").Parse(
		`Materializing from [{{.PackageName}}](https://example.com/{{.PackageName}}).

{{.Version}} — see ` + "`{{.Commit}}`" + ` for context.

---
*Generated by test-bot*`))
	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := &testData{
		PackageName: "pkg",
		Version:     "1.2.3",
		Commit:      "abcdef",
		Nested:      &testNested{Field1: "with-dashes-and_underscores", Field2: 99},
	}
	rendered, err := cm.render(bodyTmpl, want)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body, err := cm.templateExecutor.Embed(rendered, &embeddedData[testData]{Data: *want})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	got, err := cm.Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.PackageName != want.PackageName || got.Nested == nil || got.Nested.Field2 != want.Nested.Field2 {
		t.Errorf("Extract = %+v, want %+v", got, want)
	}
}
