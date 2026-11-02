/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package pathpatterns

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

type result struct {
	Key string
	OK  bool
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		exclude []string
		// want maps each path to the result Match should return for it.
		want map[string]result
	}{{
		name:    "single pattern matches all files",
		include: []string{`(.+)`},
		want: map[string]result{
			"file.txt":                 {"file.txt", true},
			"dir/file.yaml":            {"dir/file.yaml", true},
			"deep/nested/path/file.go": {"deep/nested/path/file.go", true},
			"":                         {},
		},
	}, {
		name:    "yaml files only",
		include: []string{`(.+\.yaml)`},
		want: map[string]result{
			"config.yaml":           {"config.yaml", true},
			"dir/config.yaml":       {"dir/config.yaml", true},
			"deep/nested/file.yaml": {"deep/nested/file.yaml", true},
			"file.txt":              {},
			"file.yml":              {},
			"yaml":                  {},
		},
	}, {
		name:    "specific directory",
		include: []string{`(infrastructure/.+)`},
		want: map[string]result{
			"infrastructure/main.tf":        {"infrastructure/main.tf", true},
			"infrastructure/nested/file.go": {"infrastructure/nested/file.go", true},
			"main.tf":                       {},
			"src/infrastructure/main.tf":    {},
			"infrastructure":                {},
		},
	}, {
		name:    "multiple include patterns",
		include: []string{`(.+\.yaml)`, `(configs/.+)`},
		want: map[string]result{
			"file.yaml":                  {"file.yaml", true},
			"dir/file.yaml":              {"dir/file.yaml", true},
			"configs/app.json":           {"configs/app.json", true},
			"configs/nested/config.toml": {"configs/nested/config.toml", true},
			"file.txt":                   {},
			"other/file.txt":             {},
		},
	}, {
		name:    "first matching include pattern gives the key",
		include: []string{`(configs)/.+\.yaml`, `(configs/.+)`},
		want: map[string]result{
			"configs/app.yaml": {"configs", true},
			"configs/app.json": {"configs/app.json", true},
		},
	}, {
		name:    "pattern is anchored at the start",
		include: []string{`(test.*)`},
		want: map[string]result{
			"test":              {"test", true},
			"test123":           {"test123", true},
			"testing":           {"testing", true},
			"test-suffix-extra": {"test-suffix-extra", true},
			"prefix-test":       {},
		},
	}, {
		name:    "pattern is anchored at the end",
		include: []string{`(.+\.yaml)`},
		want: map[string]result{
			"file.yaml":        {"file.yaml", true},
			"dir/file.yaml":    {"dir/file.yaml", true},
			"prefix-file.yaml": {"prefix-file.yaml", true},
			"file.yaml-extra":  {},
			"file.txt":         {},
		},
	}, {
		name:    "pattern with explicit anchors",
		include: []string{`^(.+\.yaml)$`},
		want: map[string]result{
			"file.yaml":       {"file.yaml", true},
			"file.yaml-extra": {},
		},
	}, {
		name:    "capture inside a directory prefix",
		include: []string{`dir/(.+\.go)`},
		want: map[string]result{
			"dir/main.go":        {"main.go", true},
			"dir/nested/main.go": {"nested/main.go", true},
			"main.go":            {},
			"other/main.go":      {},
		},
	}, {
		name:    "capture filename only",
		include: []string{`.+/([^/]+)`},
		want: map[string]result{
			"dir/file.txt": {"file.txt", true},
		},
	}, {
		name:    "capture directory",
		include: []string{`(.*)/[^/]+`},
		want: map[string]result{
			"bots/skillup/main.go": {"bots/skillup", true},
			"solo-file":            {},
		},
	}, {
		name:    "empty capture is no match",
		include: []string{`(.*)/[^/]+`},
		want: map[string]result{
			"/file": {},
		},
	}, {
		name:    "empty capture stops at the first matching include pattern",
		include: []string{`(a*)b`, `(.+)`},
		want: map[string]result{
			"b":  {},
			"ab": {"a", true},
		},
	}, {
		name:    "exclude pattern skips a path",
		include: []string{`(.*)/[^/]+`},
		exclude: []string{`.*/testdata/.*`},
		want: map[string]result{
			"bots/skillup/main.go":                            {"bots/skillup", true},
			"ecosystems/eco-fix-review/testdata/foo/bar.yaml": {},
			"pkg/testdata/input.json":                         {},
		},
	}, {
		name:    "exclude pattern is anchored",
		include: []string{`(.+)`},
		exclude: []string{`testdata/.*`},
		want: map[string]result{
			"testdata/a.json":     {},
			"pkg/testdata/a.json": {"pkg/testdata/a.json", true},
		},
	}, {
		name:    "exclude pattern may have capture groups",
		include: []string{`(bots/[^/]+)/.*\.go`},
		exclude: []string{`.*/(testdata|vendor)/.*`},
		want: map[string]result{
			"bots/skillup/main.go":          {"bots/skillup", true},
			"bots/skillup/testdata/foo.go":  {},
			"bots/skillup/vendor/x/y/z.go":  {},
			"bots/skillup/internal/main.go": {"bots/skillup", true},
		},
	}, {
		name:    "any exclude pattern skips a path",
		include: []string{`(.+)`},
		exclude: []string{`a/.*`, `b/.*`},
		want: map[string]result{
			"a/x": {},
			"b/x": {},
			"c/x": {"c/x", true},
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(tt.include, tt.exclude)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got := make(map[string]result, len(tt.want))
			for path := range tt.want {
				key, ok := m.Match(path)
				got[path] = result{key, ok}
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Match (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestNewError(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		exclude []string
		want    string
	}{{
		name: "no include patterns",
		want: "no include patterns",
	}, {
		name:    "empty include patterns",
		include: []string{},
		exclude: []string{`(.+)`},
		want:    "no include patterns",
	}, {
		name:    "include pattern with no capture group",
		include: []string{`.+\.yaml`},
		want:    `regex "^.+\\.yaml$" must have exactly one capture group, got 0`,
	}, {
		name:    "include pattern with two capture groups",
		include: []string{`(.+)/(.+)`},
		want:    `regex "^(.+)/(.+)$" must have exactly one capture group, got 2`,
	}, {
		name:    "later include pattern with no capture group",
		include: []string{`(.+)`, `.+`},
		want:    `regex "^.+$" must have exactly one capture group, got 0`,
	}, {
		name:    "invalid include pattern",
		include: []string{`(invalid[`},
		want:    "invalid regex \"^(invalid[$\": error parsing regexp: missing closing ]: `[$`",
	}, {
		name:    "invalid exclude pattern",
		include: []string{`(.+)`},
		exclude: []string{`(invalid[`},
		want:    "exclude pattern: invalid regex \"^(invalid[$\": error parsing regexp: missing closing ]: `[$`",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(tt.include, tt.exclude)
			if err == nil {
				t.Fatalf("New: got = %v, want error %q", m, tt.want)
			}
			if diff := cmp.Diff(tt.want, err.Error()); diff != "" {
				t.Errorf("New error (-want, +got):\n%s", diff)
			}
		})
	}
}
