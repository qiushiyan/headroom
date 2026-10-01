package accounts

import (
	"path/filepath"

	"github.com/qiushiyan/headroom/internal/config"
)

// Home is another home on this machine that spends against the same ledger:
// its accounts root, and what discovery finds under it. Its accounts are read
// for the sessions running on them and nothing else — a launch only ever runs
// in its own home.
type Home struct {
	Root string
	Set  Set
}

// OtherHomes is the homes that spend against self's ledger today, out of the
// ones registered there. A registration says where a home lives; whether it
// still shares is what that home's own `.ledger` says now, so a home that has
// left — or moved to another ledger — stops counting at once instead of for
// as long as its record takes to age out. The ledger's own home is a member
// by definition. self is never among them.
func OtherHomes(self config.Scope, registered []config.Scope) []Home {
	ledger := self.Ledger()
	var out []Home
	for _, r := range registered {
		if config.SameDir(r.AccountsRoot, self.AccountsRoot) || !config.SameDir(config.LedgerOf(r.AccountsRoot), ledger) {
			continue
		}
		out = append(out, Home{Root: filepath.Clean(r.AccountsRoot), Set: Discover(r)})
	}
	return out
}
