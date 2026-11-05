package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/state"
)

// AdoptRefs moves each vendor's records written under the primary's name to
// the primary's ref, once per home: builds before refs recorded the primary
// by a name that a pin or a login could change (state.Store.AdoptRefs). It is
// the one-time adoption, retired once every home has run it. A run that
// cannot move them says so on w, and the next run tries again.
func AdoptRefs(cfg config.Config, w io.Writer) {
	adoptRefs(cfg.Claude, w)
	adoptRefs(cfg.Codex, w)
}

func adoptRefs(scope config.Scope, w io.Writer) {
	// Only a home holding something a build before refs wrote takes the lock:
	// a fresh or absent home is left exactly as it is.
	st := state.Open(scope)
	if !st.AdoptionPending() && !exists(scope.ReservedFile()) && !exists(headless.Path(scope.AccountsRoot)) {
		return
	}
	names := legacyPrimaryNames(scope, st)
	ad, err := st.AdoptRefs(names, func() (int, error) {
		// The token file is read first: one that refuses (others can read it)
		// stops the move before anything is written.
		tokens := exists(headless.Path(scope.AccountsRoot))
		if tokens {
			if _, err := headless.Read(scope.AccountsRoot); err != nil {
				return 0, err
			}
		}
		n := 0
		moved, err := accounts.AdoptReservationRefs(scope, names)
		if err != nil {
			return n, err
		}
		if moved {
			n++
		}
		if tokens {
			moved, err := headless.AdoptRefs(scope.AccountsRoot, names, accounts.PrimaryRef)
			if err != nil {
				return n, err
			}
			if moved {
				n++
			}
		}
		return n, nil
	})
	label := vendorLabel(scope.Vendor)
	switch {
	case errors.Is(err, state.ErrReadOnly), errors.Is(err, state.ErrBusy):
		// A newer headroom owns the file, or another run holds the lock and
		// is adopting now; the next run looks again.
	case err != nil:
		fmt.Fprintf(w, "headroom: %s's primary's records could not be moved to its dir's name (%v); the next headroom command tries again\n", label, err)
	case ad.Moved():
		fmt.Fprintf(w, "headroom: %s's primary's records now name its dir, whatever it is called; moved %s from %s\n",
			label, adoptedSummary(ad), strings.Join(names, ", "))
	}
}

// legacyPrimaryNames is every name a build before refs could have written
// the primary's records under: the name and former names its home recorded,
// the pin, the login's local part, and "primary", which a home pinned until a
// host file named it. A name that is an extra's dir is that extra's.
func legacyPrimaryNames(scope config.Scope, st *state.Store) []string {
	set := accounts.Discover(scope)
	extras := map[string]bool{}
	var email string
	for _, a := range set.Accounts {
		if a.IsPrimary() {
			email = a.Email
		} else {
			extras[a.Name] = true
		}
	}
	unpinned := scope
	unpinned.PrimaryName = ""
	var names []string
	for _, n := range append(st.LegacyNames(), scope.PrimaryName, accounts.PrimaryName(unpinned, email), "primary") {
		if n != "" && n != accounts.PrimaryRef && !extras[n] && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func adoptedSummary(a state.Adopted) string {
	var parts []string
	switch {
	case a.ReHomes == 1:
		parts = append(parts, "1 session re-home")
	case a.ReHomes > 1:
		parts = append(parts, fmt.Sprintf("%d session re-homes", a.ReHomes))
	}
	if a.Last {
		parts = append(parts, "the last launch")
	}
	if a.Files > 0 {
		parts = append(parts, "the records beside the store (.reserved, headless tokens)")
	}
	return strings.Join(parts, ", ")
}

func vendorLabel(v config.Vendor) string {
	if v == config.Codex {
		return "Codex"
	}
	return "Claude Code"
}
