/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"fmt"
	"os"
	"regexp"

	gogit "github.com/go-git/go-git/v5"
	"gopkg.in/yaml.v3"
)

// repoConfig holds per-repo configuration loaded from
// .{identity}.yaml at the root of a target repository.
type repoConfig struct {
	Mode            *Mode    `yaml:"mode,omitempty"`
	ExcludePatterns []string `yaml:"exclude_patterns,omitempty"`
}

// fullRepoConfig holds the parsed and compiled per-repo configuration.
type fullRepoConfig struct {
	Mode            Mode
	excludePatterns []*regexp.Regexp
}

// isExcluded returns true if path matches any exclude pattern.
// Use this in the PR review path to skip excluded files (e.g. testdata
// fixtures) without also filtering by path_patterns — path_patterns are
// designed as trigger keys for the fix/resync path (e.g. "go.mod" as a
// module-root key), not as a scope restriction for PR review.
func (c *fullRepoConfig) isExcluded(path string) bool {
	for _, re := range c.excludePatterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// UnmarshalYAML implements yaml.Unmarshaler so Mode can be used in YAML structs.
// It reuses the same string-parsing logic as EnvDecode.
func (m *Mode) UnmarshalYAML(value *yaml.Node) error {
	return m.EnvDecode(value.Value)
}

// loadRepoConfig reads .{identity}.yaml from the root of the worktree.
// For example, a reconciler with identity "skillup" reads ".skillup.yaml".
//
//   - If the file does not exist, ModeNone is returned (the reconciler skips the repo).
//   - If the file exists but has no mode field, ModeFix is returned (safe default).
//   - If the file exists with a mode field, that mode is returned.
func loadRepoConfig(wt *gogit.Worktree, identity string) (Mode, error) {
	cfg, err := loadFullRepoConfig(wt, identity)
	if err != nil {
		return ModeNone, err
	}
	return cfg.Mode, nil
}

// loadFullRepoConfig reads .{identity}.yaml from the root of the worktree and
// returns the full configuration including compiled exclude patterns.
//
//   - If the file does not exist, a config with ModeNone is returned.
//   - If the file exists but has no mode field, ModeFix is used (safe default).
//   - exclude_patterns are compiled as anchored regexps.
func loadFullRepoConfig(wt *gogit.Worktree, identity string) (*fullRepoConfig, error) {
	path := fmt.Sprintf(".%s.yaml", identity)
	f, err := wt.Filesystem.Open(path)
	if os.IsNotExist(err) {
		return &fullRepoConfig{Mode: ModeNone}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open repo config: %w", err)
	}
	defer f.Close()

	var raw repoConfig
	if err := yaml.NewDecoder(f).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode repo config: %w", err)
	}

	mode := ModeFix
	if raw.Mode != nil {
		mode = *raw.Mode
	}

	excludePats, err := compileAnchoredPatterns(raw.ExcludePatterns)
	if err != nil {
		return nil, fmt.Errorf("compile exclude_patterns: %w", err)
	}

	return &fullRepoConfig{
		Mode:            mode,
		excludePatterns: excludePats,
	}, nil
}

// applyExcludeFilter returns a new slice containing only the paths from files
// that are not excluded by cfg's exclude_patterns. The original slice is not
// modified. Returns the original slice unchanged if cfg has no exclude patterns.
func applyExcludeFilter(files []string, cfg *fullRepoConfig) []string {
	if len(cfg.excludePatterns) == 0 {
		return files
	}
	filtered := make([]string, 0, len(files))
	for _, f := range files {
		if !cfg.isExcluded(f) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// compileAnchoredPatterns compiles a slice of regexp strings into anchored
// regexps. Returns nil for an empty slice. Each pattern is wrapped with ^ and $
// anchors so it must match the full path string.
func compileAnchoredPatterns(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		anchored := "^" + p + "$"
		re, err := regexp.Compile(anchored)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", anchored, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}
