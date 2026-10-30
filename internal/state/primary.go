package state

import (
	"errors"
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
}

// Moved says the records changed name.
func (m PrimaryMove) Moved() bool { return m.From != "" && m.From != m.To }

// RecordedPrimary is the name this home's records know its primary by, ""
// before one is recorded. A read without the lock, for the run that only
// needs to see that nothing changed: RenamePrimary decides again under it.
func (s *Store) RecordedPrimary() string { return read(s.root).primary }

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
			home.primary = to
			home.dirty = true
			mv.Recorded = true
		}
		// The ledger goes first: the home's document recording the new name is
		// the commit, and a run stopped before it moves again what is left.
		return false, nil
	})
	return mv, err
}
