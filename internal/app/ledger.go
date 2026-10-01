package app

// `headroom accounts ledger`: which subscription ledger this home spends
// against, and which homes spend against it with it. Given an accounts root,
// this home joins that root's ledger first — the one act that makes a second
// home on this machine (an automated pipeline's, say) count the first home's
// sessions and launches, and ask each subscription once per spacing with it.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

func runAccountsLedger(cfg config.Config, args []string) int {
	return runAccountsLedgerTo(os.Stdout, os.Stderr, cfg, args, time.Now())
}

// runAccountsLedgerTo registers this home in the ledger it uses — joining the
// named one first, when there is one — and lists the homes registered there.
// Registering is a no-op when this home's record is current; it is done here
// so a home that has just joined is counted by the others before its first
// launch.
func runAccountsLedgerTo(out, errw io.Writer, cfg config.Config, args []string, now time.Time) int {
	scope := cfg.Claude
	switch {
	case len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")):
		fmt.Fprintln(errw, "usage: headroom accounts ledger [<accounts root>]")
		return 2
	case len(args) == 1:
		root, err := accounts.ShareLedger(cfg.Claude, cfg.Codex, args[0])
		if err != nil {
			fmt.Fprintf(errw, "headroom accounts ledger: %v\n", err)
			return 1
		}
		scope.LedgerRoot = root
	}
	st := state.Open(scope)
	if err := st.Register(now); err != nil {
		fmt.Fprintf(errw, "headroom accounts ledger: this home could not be registered in %s (%v)\n",
			filepath.Join(st.Ledger(), "state.json"), err)
		return 1
	}
	snap := st.Load()
	whose := "this home's own"
	if snap.Shared() {
		whose = "shared — this home spends against another home's ledger"
	}
	fmt.Fprintf(out, "ledger  %s — %s\n", filepath.Join(st.Ledger(), "state.json"), whose)
	members := snap.Members()
	labels := homeLabels(snap, st.Home())
	counted := map[string]bool{st.Home(): true}
	for _, h := range otherHomes(scope, snap) {
		counted[h.Root] = true
	}
	fmt.Fprintf(out, "homes   %d registered\n", len(members))
	nameW, rootW := 0, 0
	for _, m := range members {
		nameW = max(nameW, render.Cells(render.Sanitize(homeLabel(labels, m.Root))))
		rootW = max(rootW, render.Cells(render.Sanitize(m.Root)))
	}
	for _, m := range members {
		note := ""
		if m.Root == st.Home() {
			note = "this home · "
		}
		if !counted[m.Root] {
			// Registered here once, spending elsewhere now: its record ages
			// out, and nothing counts it meanwhile.
			note += "left — not counted · "
		}
		if m.Explicit {
			note += "primary by its dir · "
		}
		if age := render.Age(now.Unix() - m.SeenAtMS/1000); age == "just now" {
			note += "seen just now"
		} else {
			note += "seen " + age + " ago"
		}
		fmt.Fprintf(out, "  %s  %s  %s\n", render.PadCell(render.Sanitize(homeLabel(labels, m.Root)), nameW),
			render.PadCell(render.Sanitize(m.Root), rootW), note)
	}
	return 0
}

// version is set at link time when a build names itself
// (-ldflags "-X github.com/qiushiyan/headroom/internal/app.version=<v>");
// otherwise the commit the binary was built from names it.
var version = ""

// versionLine is `headroom version`: one line, the build's name — the commit
// it was built from, "+dirty" when the tree had uncommitted changes — and the
// commit's time. What a caller pins.
func versionLine() string {
	rev, at, dirty := "", "", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				at = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	v := version
	if v == "" {
		v = rev
		if v == "" {
			v = "unknown"
		}
		if dirty {
			v += "+dirty"
		}
	}
	line := "headroom " + v
	if at != "" {
		line += " " + at
	}
	return line
}
