package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/qiushiyan/headroom/internal/sessions"
)

// importLegacy runs only before the checkpoint exists. The checkpoint, every
// re-home, and the maximum old deadline commit together under the store lock.
// Unreadable re-homes block checkpointing; a damaged ledger recovers quiet.
// Load may preview an import, but only a successful mutation checkpoints it.
func (d *doc) importLegacy(root string) {
	if raw, ok := d.raw["legacy_import"]; ok {
		var imported legacyImport
		if err := json.Unmarshal(raw, &imported); err != nil || string(raw) == "null" {
			d.migrationErr = fmt.Errorf("legacy import checkpoint unreadable")
			d.problems = append(d.problems, Problem{"legacy_import", d.migrationErr.Error()})
		} else {
			d.imported = &imported
		}
		return
	}
	if d.readOnly() {
		return
	}
	imported := &legacyImport{}
	fail := func(name string, err error) {
		d.migrationErr = fmt.Errorf("legacy import %s: %w", name, err)
		d.problems = append(d.problems, Problem{"legacy_import", d.migrationErr.Error()})
	}
	throttle, err := os.ReadFile(filepath.Join(root, ".throttle"))
	if !os.IsNotExist(err) {
		var entries map[string]struct {
			NextEligibleMS int64 `json:"next_eligible_ms"`
		}
		if err != nil || json.Unmarshal(throttle, &entries) != nil || entries == nil {
			// A lost ledger cannot prove any account is eligible. One maximum
			// cooldown protects every old budget, then retires the bad input.
			// Re-homes below remain independently readable.
			imported.QuietUntilMS = time.Now().Add(CooldownMax).UnixMilli()
		} else {
			for _, r := range entries {
				if r.NextEligibleMS > imported.QuietUntilMS {
					imported.QuietUntilMS = r.NextEligibleMS
				}
			}
		}
	}
	owners, err := os.ReadFile(filepath.Join(root, ".owners"))
	if err != nil && !os.IsNotExist(err) {
		d.badSessions = true
		fail(".owners", err)
		return
	}
	if err == nil {
		var old struct {
			Owners map[string]sessions.OwnerRec `json:"owners"`
		}
		if err := json.Unmarshal(owners, &old); err != nil || old.Owners == nil || !validOwners(old.Owners) {
			d.badSessions = true
			fail(".owners", ErrCorrupt)
			return
		}
		if d.badSessions {
			fail("state sessions", ErrCorrupt)
			return
		}
		mergeOwners(d.sessions, old.Owners)
	}
	if max := time.Now().Add(CooldownMax).UnixMilli(); imported.QuietUntilMS > max {
		imported.QuietUntilMS = max
	}
	d.imported = imported
	d.dirty = true
}

// Archives are recovery copies, never inputs. A crash after commit but before
// rename is harmless: the checkpoint prevents importing twice. Old concurrent
// writers are outside the migration contract.
func (s *Store) archiveLegacy(d *doc) {
	if d.imported == nil {
		return
	}
	if _, checkpointed := d.raw["legacy_import"]; checkpointed {
		return
	}
	for _, name := range []string{".throttle", ".owners"} {
		path := filepath.Join(s.root, name)
		// Preserve the first recovery copy if a retired writer recreates its file.
		if _, err := os.Stat(path + ".imported"); os.IsNotExist(err) {
			_ = os.Rename(path, path+".imported")
		}
	}
}
