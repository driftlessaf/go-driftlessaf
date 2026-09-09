/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus

import (
	"fmt"
	"strings"
)

// ValidateSupersededKeys checks that each key in superseded is safe for a
// [Surface] to delete by prefix given botKey and the active EntryKey.
//
// A [Surface.RemoveSuperseded] implementation deletes by prefix match, so a
// key that is too broad deletes entries the design preserves rather than
// only the legacy entries it was meant to clean up. ValidateSupersededKeys
// rejects a key that is:
//
//   - empty, which would match every bot-authored entry on a resource;
//   - not prefixed with "botKey:", which would match another bot's
//     entries; or
//   - a prefix of active, which would match the active entry this update
//     is publishing.
//
// botKey and active anchor every check above, so both are checked too: a
// nonempty botKey, and an active key prefixed with "botKey:" and longer.
func ValidateSupersededKeys(botKey string, active EntryKey, superseded ...EntryKey) error {
	if botKey == "" {
		return fmt.Errorf("bot key is empty")
	}
	prefix := botKey + ":"
	if !strings.HasPrefix(string(active), prefix) || string(active) == prefix {
		return fmt.Errorf("active entry key %q is not an entry of bot key %q", active, botKey)
	}
	for _, key := range superseded {
		switch {
		case key == "":
			return fmt.Errorf("superseded key is empty")
		case !strings.HasPrefix(string(key), prefix):
			return fmt.Errorf("superseded key %q is not prefixed with bot key %q", key, botKey)
		case strings.HasPrefix(string(active), string(key)):
			return fmt.Errorf("superseded key %q is a prefix of the active entry key %q", key, active)
		}
	}
	return nil
}
