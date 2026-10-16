/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"errors"
	"testing"
)

type suffixRequest struct{ lens string }

type lensXML struct {
	Text string `xml:",chardata"`
}

func (r suffixRequest) Bind(p *Prompt) (*Prompt, error) { return p, nil }

func (r suffixRequest) BindSuffix(p *Prompt) (*Prompt, error) {
	if r.lens == "" {
		return nil, errors.New("no lens")
	}
	return p.BindXML("lens", lensXML{Text: r.lens})
}

func TestBindSuffix(t *testing.T) {
	suffix := MustNewPrompt("lens {{lens}}")
	for _, tc := range []struct {
		name    string
		suffix  *Prompt
		request any
		want    string
		wantErr bool
		wantNil bool
	}{
		{name: "bindable request", suffix: suffix, request: suffixRequest{lens: "a<b"}, want: "lens <lensXML>a&lt;b</lensXML>"},
		{name: "bind error", suffix: suffix, request: suffixRequest{}, wantErr: true},
		{name: "other request leaves the suffix", suffix: MustNewPrompt("static"), request: Noop{}, want: "static"},
		{name: "nil suffix", request: suffixRequest{lens: "x"}, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BindSuffix(tc.suffix, tc.request)
			if (err != nil) != tc.wantErr {
				t.Fatalf("BindSuffix: got err = %v, want error = %t", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.wantNil {
				if got != nil {
					t.Errorf("BindSuffix: got %v, want nil", got)
				}
				return
			}
			text, err := got.Build()
			if err != nil || text != tc.want {
				t.Errorf("Build: got = %q, %v; want = %q", text, err, tc.want)
			}
		})
	}
}
