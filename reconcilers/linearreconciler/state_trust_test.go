/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"strings"
	"testing"
	"time"
)

// TestStateManager_LoadTrustsOnlyBotAttachments pins that Load reads state only
// from attachments the bot created. Anyone who can edit an issue can add an
// attachment titled "<prefix>_state"; before this check the newest one won,
// so a forged attachment could rewrite the bot's phase, project, or target
// repository.
func TestStateManager_LoadTrustsOnlyBotAttachments(t *testing.T) {
	const (
		trusted = `{"location":"Bridge","health":100}`
		forged  = `{"location":"Airlock","health":1}`
	)
	now := time.Now()

	tests := []struct {
		name         string
		attachments  func(base string) []Attachment
		wantFound    bool
		wantErr      string
		wantLocation string
	}{{
		name: "newer forged attachment is ignored",
		attachments: func(base string) []Attachment {
			return []Attachment{
				{ID: "bot", Title: "test_state", URL: base + "/trusted.json", CreatedAt: now.Add(-time.Hour), Creator: &User{ID: stateTestBotID}},
				{ID: "forged", Title: "test_state", URL: base + "/forged.json", CreatedAt: now, Creator: &User{ID: "attacker"}},
			}
		},
		wantFound:    true,
		wantLocation: "Bridge",
	}, {
		name: "only a forged attachment is an error, not a fresh start",
		attachments: func(base string) []Attachment {
			return []Attachment{
				{ID: "forged", Title: "test_state", URL: base + "/forged.json", CreatedAt: now, Creator: &User{ID: "attacker"}},
			}
		},
		wantErr: "none was created by a trusted creator",
	}, {
		name: "an attachment with no creator is untrusted",
		attachments: func(base string) []Attachment {
			return []Attachment{
				{ID: "nocreator", Title: "test_state", URL: base + "/forged.json", CreatedAt: now},
			}
		},
		wantErr: "none was created by a trusted creator",
	}, {
		name: "no state attachment is no state",
		attachments: func(base string) []Attachment {
			return []Attachment{{ID: "other", Title: "other_state", URL: base + "/forged.json", Creator: &User{ID: "attacker"}}}
		},
		wantFound: false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStateTestServer(t, map[string][]byte{
				"/trusted.json": []byte(trusted),
				"/forged.json":  []byte(forged),
			})
			defer srv.Close()

			// Built without New, as tidy-upper builds its client: Load must
			// resolve the bot user from the viewer query itself.
			client := NewClientWithAPIKey("test-token").
				WithHTTPClient(srv.Client()).
				WithEndpoint(srv.URL)
			client.statePrefix = "test"

			issue := &Issue{ID: "issue-1"}
			issue.Attachments.Nodes = tc.attachments(srv.URL)

			var state testBotState
			found, err := client.NewStateManager(issue).Load(t.Context(), &state)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() error = %v, want one containing %q", err, tc.wantErr)
				}
				if state.Location != "" {
					t.Errorf("Load() populated state %+v from an untrusted attachment", state)
				}
				return
			case err != nil:
				t.Fatalf("Load() error = %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("Load() found = %v, want %v", found, tc.wantFound)
			}
			if state.Location != tc.wantLocation {
				t.Errorf("Location = %q, want %q", state.Location, tc.wantLocation)
			}
		})
	}
}

// TestStateManager_LoadTrustsConfiguredCreators covers a bot that operators
// run under their own identities: a run as one operator must read state the
// other operator wrote, while a stranger's attachment is still refused.
func TestStateManager_LoadTrustsConfiguredCreators(t *testing.T) {
	const (
		operatorState = `{"location":"Bridge","health":100}`
		forged        = `{"location":"Airlock","health":1}`
	)
	now := time.Now()

	tests := []struct {
		name         string
		attachments  func(base string) []Attachment
		wantErr      string
		wantLocation string
	}{{
		name: "state written by another trusted operator loads",
		attachments: func(base string) []Attachment {
			return []Attachment{
				{ID: "op", Title: "test_state", URL: base + "/trusted.json", CreatedAt: now, Creator: &User{ID: "operator-2"}},
			}
		},
		wantLocation: "Bridge",
	}, {
		name: "newer stranger attachment is ignored over a trusted operator's",
		attachments: func(base string) []Attachment {
			return []Attachment{
				{ID: "op", Title: "test_state", URL: base + "/trusted.json", CreatedAt: now.Add(-time.Hour), Creator: &User{ID: "operator-2"}},
				{ID: "forged", Title: "test_state", URL: base + "/forged.json", CreatedAt: now, Creator: &User{ID: "attacker"}},
			}
		},
		wantLocation: "Bridge",
	}, {
		name: "only a stranger's attachment is still an error",
		attachments: func(base string) []Attachment {
			return []Attachment{
				{ID: "forged", Title: "test_state", URL: base + "/forged.json", CreatedAt: now, Creator: &User{ID: "attacker"}},
			}
		},
		wantErr: "none was created by a trusted creator",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStateTestServer(t, map[string][]byte{
				"/trusted.json": []byte(operatorState),
				"/forged.json":  []byte(forged),
			})
			defer srv.Close()

			client := NewClientWithAPIKey("test-token").
				WithHTTPClient(srv.Client()).
				WithEndpoint(srv.URL).
				WithTrustedStateCreators(" operator-2 ", "")
			client.statePrefix = "test"

			issue := &Issue{ID: "issue-1"}
			issue.Attachments.Nodes = tc.attachments(srv.URL)

			var state testBotState
			_, err := client.NewStateManager(issue).Load(t.Context(), &state)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if state.Location != tc.wantLocation {
				t.Errorf("Location = %q, want %q", state.Location, tc.wantLocation)
			}
		})
	}
}

// TestClient_ResolveBotUserID checks the lazy lookup used by clients that
// were not built through New, and that a value New already set is kept.
func TestClient_ResolveBotUserID(t *testing.T) {
	srv := newStateTestServer(t, nil)
	defer srv.Close()

	client := NewClientWithAPIKey("test-token").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)
	got, err := client.ResolveBotUserID(t.Context())
	if err != nil {
		t.Fatalf("ResolveBotUserID() error = %v", err)
	}
	if got != stateTestBotID || client.BotUserID != stateTestBotID {
		t.Errorf("ResolveBotUserID() = %q (BotUserID %q), want %q", got, client.BotUserID, stateTestBotID)
	}

	client.BotUserID = "preset"
	if got, _ := client.ResolveBotUserID(t.Context()); got != "preset" {
		t.Errorf("ResolveBotUserID() = %q, want the preset %q", got, "preset")
	}
}
