package state

import (
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
)

// A one-time adoption, retired once every home has run it: builds before
// refs wrote the primary's records under its display name, which a pin or a
// login could change. A home's records now name it accounts.PrimaryRef, and
// the records written before are moved there once.

// refsSection marks a home whose records name its primary by
// accounts.PrimaryRef: present once the adoption committed. legacySection is
// what builds before refs recorded the name those records were under.
const (
	refsSection   = "refs"
	legacySection = "primary"
	adoptWait     = 2 * time.Second
)

// errAdoptReHomesUnreadable refuses an adoption that would leave re-homes it
// cannot read under the old name.
var errAdoptReHomesUnreadable = errors.New("the session re-homes cannot be read, so they cannot move")

// Adopted is what AdoptRefs did.
type Adopted struct {
	ReHomes int  // session re-homes moved to the ref
	Last    bool // this home's --last moved to the ref
	Files   int  // records beside the store moved: reservations, tokens
}

// Moved says any record changed.
func (a Adopted) Moved() bool { return a.ReHomes > 0 || a.Last || a.Files > 0 }

// AdoptionPending says the home has a document a build before refs may have
// written and that does not yet say its records name the primary by its
// ref; a read without the lock, for the run that only needs to see there is
// nothing to do. A home with no document has nothing in it to adopt.
func (s *Store) AdoptionPending() bool {
	d := read(s.root)
	_, done := d.raw[refsSection]
	return !done && (len(d.raw) > 0 || d.unreadable || d.corruptDoc)
}

// LegacyNames is every name the home's legacy section says its primary's
// records were under — the recorded name, then the former ones — or nil
// when there is none; a read without the lock.
func (s *Store) LegacyNames() []string {
	var rec struct {
		Name   string   `json:"name"`
		Former []string `json:"former"`
	}
	b, ok := read(s.root).raw[legacySection]
	if !ok || json.Unmarshal(b, &rec) != nil {
		return nil
	}
	return slices.DeleteFunc(append([]string{rec.Name}, rec.Former...), func(n string) bool { return n == "" })
}

// AdoptRefs moves this home's records named by any of names — its session
// re-homes and its --last — to accounts.PrimaryRef, under the store's
// locks, so two runs that both see the home unadopted move it once. files
// moves the records kept beside the store (`.reserved`, headless tokens) and
// runs first; the home's document recording the marker, and dropping the
// legacy section, is the commit, so a run stopped partway is finished by the
// next, which finds the files already moved.
//
// A home with no document gets none just to say so: it has no records, and
// the run that first writes one adopts it then. Only this home's --last
// moves: a shared ledger's other entries are other homes', named by them.
func (s *Store) AdoptRefs(names []string, files func() (int, error)) (Adopted, error) {
	var out Adopted
	err := s.updatePair(adoptWait, func(led, home *doc, homeErr error) (bool, error) {
		if homeErr != nil {
			return false, homeErr
		}
		if _, done := home.raw[refsSection]; done {
			return false, nil
		}
		if home.badSessions {
			return false, errAdoptReHomesUnreadable
		}
		n, err := files()
		if err != nil {
			return false, err
		}
		out.Files = n
		for id, rec := range home.sessions {
			if slices.Contains(names, rec.Account) {
				rec.Account = accounts.PrimaryRef
				home.sessions[id] = rec
				out.ReHomes++
			}
		}
		if !led.badPlacements {
			if last, ok := led.placements.ByHome[s.root]; ok && slices.Contains(names, last.Ref) {
				last.Ref = accounts.PrimaryRef
				led.placements.ByHome[s.root] = last
				led.dirty, out.Last = true, true
			}
		}
		if len(home.raw) == 0 && !out.Moved() {
			return false, nil
		}
		home.raw[refsSection] = json.RawMessage("1")
		delete(home.raw, legacySection)
		home.dirty = true
		// The ledger goes first: the home's document recording the marker is
		// the commit, and a run stopped before it adopts again what is left.
		return false, nil
	})
	return out, err
}
