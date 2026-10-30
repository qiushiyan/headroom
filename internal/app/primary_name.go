package app

import (
	"errors"
	"fmt"
	"io"
	"os"
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
// Only a pinned name is settled: a derived one follows whoever is logged in,
// and is documented as no steadier than that login. The common run reads the
// recorded name, finds it unchanged and does nothing more.
func SettlePrimaryNames(cfg config.Config, w io.Writer) {
	for _, scope := range []config.Scope{cfg.Claude, cfg.Codex} {
		settlePrimaryName(scope, w)
	}
}

func settlePrimaryName(scope config.Scope, w io.Writer) {
	to := scope.PrimaryName
	if to == "" {
		return
	}
	st := state.Open(scope)
	if st.RecordedPrimary() == to {
		return
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
	if extras[to] {
		fmt.Fprintf(w, "headroom: %s's primary is named %s, which is also an extra account's dir — its records stay where they are until the name is one no other account has\n", label, to)
		return
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
	case mv.Moved() && mv.ReHomes+mv.Placements+mv.Files > 0:
		fmt.Fprintf(w, "headroom: %s's primary is now %s; moved its records from %s (%s)\n", label, to, mv.From, movedSummary(mv))
	}
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
