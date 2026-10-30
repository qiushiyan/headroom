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

// SettlePrimaryNames keeps each vendor's records under the name its primary
// answers to now. `.current`, re-homes, reservations, --last and headless
// tokens are written under the primary's name, so a host file that pins
// another (HEADROOM_PRIMARY_NAME, HEADROOM_CODEX_PRIMARY_NAME) would strand
// them — `.current` refusing, re-homes pointing at an account that no longer
// exists. A changed pin moves them, once, under the store's lock, and says so
// on w; changing the host file is all a person does.
//
// A pin naming a name the primary had before the recorded one comes from a
// shell started before a rename: it moves nothing, and the run goes on under
// the recorded name, so the records never flip back and forth between old and
// new shells (state.FormerNames says why the newest name wins).
//
// It returns the configuration the run uses: each scope's PrimaryName is the
// name the records are under, and PrimaryAliases its former names, which
// still select the primary for a launcher an old shell defined.
//
// Only a pinned name is settled: a derived one follows whoever is logged in,
// and is documented as no steadier than that login. The common run reads the
// recorded name, finds it unchanged and does nothing more.
func SettlePrimaryNames(cfg config.Config, w io.Writer) config.Config {
	cfg.Claude = settlePrimaryName(cfg.Claude, w)
	cfg.Codex = settlePrimaryName(cfg.Codex, w)
	return cfg
}

func settlePrimaryName(scope config.Scope, w io.Writer) config.Scope {
	to := scope.PrimaryName
	if to == "" {
		return scope
	}
	st := state.Open(scope)
	recorded := st.RecordedPrimary()
	if recorded == to {
		scope.PrimaryAliases = st.RecordedFormer()
		return scope
	}

	// Nothing recorded yet: the records were written under the name the
	// primary had before any was pinned, which every pin so far has kept —
	// its login's local part, or "primary" before a login. A name that is an
	// extra's dir is never one the primary's records were written under.
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
	inferred := accounts.PrimaryName(unpinned, email)
	label := vendorLabel(scope.Vendor)
	// A stale shell's pin: the run uses the recorded name, quietly, since every
	// pane open at the rename runs into this until it is restarted.
	stale := func() config.Scope {
		scope.PrimaryName = st.RecordedPrimary()
		scope.PrimaryAliases = state.FormerNames(scope.PrimaryName, st.RecordedFormer(), inferred)
		return scope
	}
	if recorded != "" && slices.Contains(state.FormerNames(recorded, st.RecordedFormer(), inferred), to) {
		return stale()
	}
	if extras[to] {
		fmt.Fprintf(w, "headroom: %s's primary is named %s, which is also an extra account's dir — its records stay where they are until the name is one no other account has\n", label, to)
		return scope
	}

	mv, err := st.RenamePrimary(inferred, to, func(from string) (int, error) {
		if extras[from] {
			return 0, fmt.Errorf("%s is an extra account's name now, and its records are that account's", from)
		}
		// The token file is read first: one that refuses (others can read it)
		// stops the move before anything is written.
		_, statErr := os.Stat(headless.Path(scope.AccountsRoot))
		tokens := statErr == nil
		if tokens {
			if _, err := headless.Read(scope.AccountsRoot); err != nil {
				return 0, err
			}
		}
		n, err := accounts.RenamePrimaryFiles(scope, from, to)
		if err != nil {
			return n, err
		}
		if tokens {
			moved, err := headless.Rename(scope.AccountsRoot, from, to)
			if err != nil {
				return n, err
			}
			if moved {
				n++
			}
		}
		return n, nil
	})
	switch {
	case errors.Is(err, state.ErrReadOnly), errors.Is(err, state.ErrBusy):
		// A newer headroom owns the file and settles the name itself; or
		// another run holds the lock, perhaps moving the records now, and the
		// next run looks again.
	case err != nil:
		fmt.Fprintf(w, "headroom: %s's primary is named %s, and its records could not be moved to it (%v); the next headroom command tries again\n", label, to, err)
	case mv.Stale:
		// Another run recorded a newer name between the read above and the lock.
		return stale()
	case mv.Moved() && mv.ReHomes+mv.Placements+mv.Files > 0:
		fmt.Fprintf(w, "headroom: %s's primary is now %s; moved its records from %s (%s)\n", label, to, mv.From, movedSummary(mv))
	}
	scope.PrimaryAliases = st.RecordedFormer()
	return scope
}

func movedSummary(mv state.PrimaryMove) string {
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(mv.Files, "record beside the store (.current, .reserved, headless tokens)", "records beside the store (.current, .reserved, headless tokens)")
	add(mv.ReHomes, "session re-home", "session re-homes")
	add(mv.Placements, "launch record", "launch records")
	return strings.Join(parts, ", ")
}

func vendorLabel(v config.Vendor) string {
	if v == config.Codex {
		return "Codex"
	}
	return "Claude Code"
}
