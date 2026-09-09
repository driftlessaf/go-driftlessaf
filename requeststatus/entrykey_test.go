/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus_test

import (
	"testing"

	"chainguard.dev/driftlessaf/requeststatus"
)

func TestValidateSupersededKeys(t *testing.T) {
	const botKey = "manifest-gen"
	const active requeststatus.EntryKey = "manifest-gen:request-status"

	tests := []struct {
		name       string
		superseded []requeststatus.EntryKey
		wantErr    bool
	}{{
		name:       "no keys",
		superseded: nil,
	}, {
		name:       "legacy key for this bot",
		superseded: []requeststatus.EntryKey{"manifest-gen:start", "manifest-gen:failure"},
	}, {
		name:       "empty key",
		superseded: []requeststatus.EntryKey{""},
		wantErr:    true,
	}, {
		name:       "key for another bot",
		superseded: []requeststatus.EntryKey{"image-gen:start"},
		wantErr:    true,
	}, {
		name:       "key missing separator matches bot key by chance",
		superseded: []requeststatus.EntryKey{"manifest-gen-legacy:start"},
		wantErr:    true,
	}, {
		name:       "bot-key-only key is a prefix of the active key",
		superseded: []requeststatus.EntryKey{"manifest-gen:"},
		wantErr:    true,
	}, {
		name:       "active key itself is a prefix of the active key",
		superseded: []requeststatus.EntryKey{active},
		wantErr:    true,
	}, {
		name:       "one valid and one invalid key",
		superseded: []requeststatus.EntryKey{"manifest-gen:start", ""},
		wantErr:    true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requeststatus.ValidateSupersededKeys(botKey, active, tt.superseded...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSupersededKeys() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateSupersededKeysAnchors pins that an invalid botKey or active
// key is rejected rather than trusted as the basis for every other check.
func TestValidateSupersededKeysAnchors(t *testing.T) {
	tests := []struct {
		name       string
		botKey     string
		active     requeststatus.EntryKey
		superseded []requeststatus.EntryKey
		wantErr    bool
	}{{
		name:       "valid anchors",
		botKey:     "manifest-gen",
		active:     "manifest-gen:request-status",
		superseded: []requeststatus.EntryKey{"manifest-gen:start"},
	}, {
		name:       "empty bot key",
		active:     "manifest-gen:request-status",
		superseded: []requeststatus.EntryKey{"manifest-gen:start"},
		wantErr:    true,
	}, {
		name:    "empty bot key with no superseded keys",
		active:  "manifest-gen:request-status",
		wantErr: true,
	}, {
		name:       "active key belongs to another bot",
		botKey:     "manifest-gen",
		active:     "image-gen:request-status",
		superseded: []requeststatus.EntryKey{"manifest-gen:request-status"},
		wantErr:    true,
	}, {
		name:       "empty active key",
		botKey:     "manifest-gen",
		superseded: []requeststatus.EntryKey{"manifest-gen:start"},
		wantErr:    true,
	}, {
		name:       "active key is the bot prefix alone",
		botKey:     "manifest-gen",
		active:     "manifest-gen:",
		superseded: []requeststatus.EntryKey{"manifest-gen:start"},
		wantErr:    true,
	}, {
		name:       "active key matches bot key by chance without separator",
		botKey:     "manifest-gen",
		active:     "manifest-gen-legacy:request-status",
		superseded: []requeststatus.EntryKey{"manifest-gen:start"},
		wantErr:    true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requeststatus.ValidateSupersededKeys(tt.botKey, tt.active, tt.superseded...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSupersededKeys() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
