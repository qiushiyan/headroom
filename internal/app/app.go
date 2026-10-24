// Package app holds headroom's surfaces: the board, the session picker, the
// launch, login, headless placement, the account set, the logs and the
// documents. Each is one typed entry taking its streams, its options and the
// edges it reaches through; cmd/headroom translates argv into one of them.
// Any per-account problem becomes that account's own status, rendered for it
// alone — accounts fail independently.
package app

import (
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/round"
)

// markBoard sets every account's load and, under auto, the account a launch
// would take from the figures the round holds, and returns what it gathered,
// so a board that stays open can mark again as time passes. The mode is the
// round's own reading of `.current`, never a second one.
func markBoard(r *round.Round, now time.Time, place launchop.Deps) *launchop.Marks {
	m := launchop.Mark(r.Set, r.Store, r.Accounts(), r.Routing.Mode, os.Environ(), now, place)
	r.Mark(m.At(now))
	return m
}

// views are the facts a renderer draws, one per account.
func views(accts []accountstate.Account) []accountstate.Facts {
	vs := make([]accountstate.Facts, len(accts))
	for i, a := range accts {
		vs[i] = a.View
	}
	return vs
}
