/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package pathpatterns

import (
	"errors"
	"fmt"
	"regexp"
)

// Matcher holds compiled include and exclude patterns.
//
// A Matcher is safe for concurrent use.
type Matcher struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// New compiles include and exclude patterns. Every pattern is anchored with ^
// and $. Each include pattern must have exactly one capture group; exclude
// patterns may have any.
//
// It returns an error for zero include patterns or an invalid pattern.
func New(include, exclude []string) (*Matcher, error) {
	if len(include) == 0 {
		return nil, errors.New("no include patterns")
	}
	m := &Matcher{
		include: make([]*regexp.Regexp, 0, len(include)),
		exclude: make([]*regexp.Regexp, 0, len(exclude)),
	}
	for _, p := range include {
		re, err := compile(p)
		if err != nil {
			return nil, err
		}
		if n := re.NumSubexp(); n != 1 {
			return nil, fmt.Errorf("regex %q must have exactly one capture group, got %d", re.String(), n)
		}
		m.include = append(m.include, re)
	}
	for _, p := range exclude {
		re, err := compile(p)
		if err != nil {
			return nil, fmt.Errorf("exclude pattern: %w", err)
		}
		m.exclude = append(m.exclude, re)
	}
	return m, nil
}

// Match returns the key for path: the capture group of the first include
// pattern that matches. ok is false when any exclude pattern matches, no
// include pattern matches, or the capture is empty.
func (m *Matcher) Match(path string) (key string, ok bool) {
	for _, re := range m.exclude {
		if re.MatchString(path) {
			return "", false
		}
	}
	for _, re := range m.include {
		if sub := re.FindStringSubmatch(path); sub != nil {
			return sub[1], sub[1] != ""
		}
	}
	return "", false
}

func compile(pattern string) (*regexp.Regexp, error) {
	anchored := "^" + pattern + "$"
	re, err := regexp.Compile(anchored)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %q: %w", anchored, err)
	}
	return re, nil
}
