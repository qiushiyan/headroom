// Package check verifies the assumptions headroom reverse-engineered from
// each vendor it finds, and audits the files headroom keeps for itself. A
// FAIL against a vendor means that vendor likely changed a format on update;
// a FAIL against headroom's own files says so instead, and the closing line
// tells the two apart. Checks go through the exact same parsers rendering
// uses, so the checker cannot drift from what rendering actually needs.
//
// One file per audited subject: the report and its verdict (report.go), each
// vendor's group (claude.go, codex.go), the session store (sessions.go),
// headroom's state files (ownstate.go), routing and homes (routing.go),
// retention (retention.go) and the installed binaries (binary.go).
package check

import (
	"fmt"
	"io"
	"os"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/sessions"
)

// Exit codes: a checker that cannot distinguish "the assumption broke" from
// "I couldn't test it" is worse than useless when the dashboard misbehaves —
// it confirms the false diagnosis. Rate limiting, a dead network and a token
// mid-refresh are all reasons no evidence was gathered, not evidence against.
const (
	ExitPass         = 0
	ExitFail         = 1
	ExitInconclusive = 2
)

// Run checks every present vendor, Claude Code first, and returns the exit
// code. probe is the process inspection that proves a registry claim live — a
// pid that runs *and* started when the claim says. It is injected because
// exec stays at the edges; nil means liveness cannot be established, and the
// assertions that need a live session are then not made.
func Run(cfg config.Config, out io.Writer, color bool, probe sessions.PIDProbe) int {
	r := newReport(out, color)
	checkClaude(cfg.Claude, os.Environ(), probe, r)

	// Codex's group runs after Claude Code's, through the same report.
	// `check` takes no --vendor: it runs every present vendor's checks, and an
	// absent Codex is one informational line that changes no exit code.
	if cfg.Codex.Present {
		r.group(config.Codex, "codex ")
		checkCodex(cfg.Codex, os.Environ(), r)
	} else {
		r.note(fmt.Sprintf("codex: not found (no %s, no %s) — Codex checks not run",
			cfg.Codex.PrimaryDir(), cfg.Codex.AccountsRoot))
	}
	return r.verdict()
}
