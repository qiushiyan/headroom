package state

import (
	"errors"
	"slices"
	"time"
)

// renameWait bounds how long a rename waits for the lock: a run that cannot
// take it leaves the move to the run holding it, or to the next.
const renameWait = 2 * time.Second

// errReHomesUnreadable refuses a rename that would leave re-homes it cannot
// read under the old name.
var errReHomesUnreadable = errors.New("the session re-homes cannot be read, so they cannot move")

// PrimaryMove is what RenamePrimary did. From is "" when there was no earlier
// name to move from — nothing recorded and nothing inferred — and equals To
// when the records were already under To.
type PrimaryMove struct {
	From, To   string
	ReHomes    int  // session re-homes moved to To
	Placements int  // this home's launch records moved to To, --last among them
	Files      int  // records beside the store that files moved: `.current`, reservations, tokens
	Recorded   bool // the document now records To where it recorded another name or none
	// Stale says To is a name the primary had before From: the pin came from
	// a shell started before a rename, and nothing moved.
	Stale bool
}

// Moved says the records changed name.
func (m PrimaryMove) Moved() bool { return m.From != "" && m.From != m.To }

// RecordedPrimary is the name this home's records know its primary by, ""
// before one is recorded. A read without the lock, for the run that only
// needs to see that nothing changed: RenamePrimary decides again under it.
func (s *Store) RecordedPrimary() string { return read(s.root).primary }

// RecordedFormer is the names the primary's records were under before the
// recorded one, oldest first; a read without the lock, as RecordedPrimary.
func (s *Store) RecordedFormer() []string { return slices.Clone(read(s.root).primaryFormer) }

// FormerNames is every name the primary had before `recorded`: the ones the
// home records, and — for a home that recorded no history, as builds before
// it did not — `inferred`, the name its records had before any was pinned,
// when the recorded name has since replaced it.
//
// A former name is never taken back by a pin. A pin cannot tell a shell
// started before the rename from a person going back to the old name, and
// the stale shells are many (every pane open at the rename) while going back
// is a rare, deliberate act: the newest name the host file gave wins. Going
// back means removing the name from the state file's primary.former first.
func FormerNames(recorded string, former []string, inferred string) []string {
	out := slices.Clone(former)
	if recorded != "" && inferred != "" && inferred != recorded && !slices.Contains(out, inferred) {
		out = append(out, inferred)
	}
	return slices.DeleteFunc(out, func(n string) bool { return n == recorded })
}

// RenamePrimary moves this home's records from the name they know its primary
// by to `to`, under the store's locks, so two runs that both see the change
// move it once. The name moved from is the recorded one, or `inferred` when
// none is recorded yet. files moves the records kept beside the store
// (`.current`, `.reserved`, headless tokens) and runs first: the document
// recording `to` is the commit, so a run that stops partway is finished by the
// next, which finds the same name still recorded and the files already moved.
//
// Re-homes are a person's decisions: a home whose re-homes cannot be read
// moves nothing and records nothing. Only this home's launch records move —
// a shared ledger holds other homes' too, whose accounts are theirs to name.
func (s *Store) RenamePrimary(inferred, to string, files func(from string) (int, error)) (PrimaryMove, error) {
	mv := PrimaryMove{To: to}
	err := s.updatePair(renameWait, func(led, home *doc, homeErr error) (bool, error) {
		if homeErr != nil {
			return false, homeErr
		}
		from := home.primary
		former := FormerNames(home.primary, home.primaryFormer, inferred)
		if home.primary != "" && slices.Contains(former, to) {
			mv.From, mv.Stale = from, true
			return false, nil
		}
		if from == "" {
			from = inferred
		}
		mv.From = from
		if from != "" && from != to {
			if home.badSessions {
				return false, errReHomesUnreadable
			}
			n, err := files(from)
			if err != nil {
				return false, err
			}
			mv.Files = n
			for id, rec := range home.sessions {
				if rec.Account == from {
					rec.Account = to
					home.sessions[id] = rec
					mv.ReHomes++
				}
			}
			if !led.badPlacements {
				if last, ok := led.placements.ByHome[s.root]; ok && last.Name == from {
					last.Name = to
					led.placements.ByHome[s.root] = last
					mv.Placements++
				}
				for i, p := range led.placements.Recent {
					if p.Home == s.root && p.Name == from {
						led.placements.Recent[i].Name = to
						mv.Placements++
					}
				}
			}
			if !led.badRuns {
				for i, r := range led.runs {
					if r.Home == s.root && r.Name == from {
						led.runs[i].Name = to
						mv.Placements++
					}
				}
			}
			if mv.Placements > 0 {
				led.dirty = true
			}
			if mv.ReHomes > 0 {
				home.dirty = true
			}
		}
		// A home with no document and nothing moved gets none just to say so.
		if home.primary != to && (home.existed || mv.ReHomes+mv.Placements+mv.Files > 0) {
			home.setAside("primary", &home.badPrimary)
			if from != "" && from != to && !slices.Contains(former, from) {
				former = append(former, from)
			}
			home.primary, home.primaryFormer = to, former
			home.dirty = true
			mv.Recorded = true
		}
		// The ledger goes first: the home's document recording the new name is
		// the commit, and a run stopped before it moves again what is left.
		return false, nil
	})
	return mv, err
}
