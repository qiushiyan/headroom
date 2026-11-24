package render

import (
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
)

func agyFacts(attempt accountstate.Attempt, obs *accountstate.Observation) accountstate.Facts {
	return accountstate.Facts{Vendor: config.Agy, Label: "primary", Launcher: "agy", LoginCommand: "agy",
		Health: accountstate.HealthOK, Attempt: attempt, Obs: obs}
}

// A read that did not answer says why in agy's terms, and never that the
// account must log in: headroom has no evidence of that.
func TestAgyReadCaptions(t *testing.T) {
	p := NewPalette(false)
	for _, c := range []struct {
		attempt accountstate.Attempt
		want    string
	}{
		{accountstate.Attempt{State: accountstate.AttemptVendorFailed, ExitCode: 2}, "usage unknown — agy is using an API key — no account quota to read"},
		{accountstate.Attempt{State: accountstate.AttemptVendorFailed, ExitCode: 1}, "usage unknown — agy exited 1 — run agy to see why"},
		{accountstate.Attempt{State: accountstate.AttemptTransport}, "usage unknown — agy did not answer — not on PATH, or timed out"},
		{accountstate.Attempt{State: accountstate.AttemptPending}, "usage unknown — fetching…"},
	} {
		text := boardText(p.Board([]accountstate.Facts{agyFacts(c.attempt, nil)}, renderNow, LayoutBlocks, 0))
		if !strings.Contains(text, c.want) {
			t.Errorf("attempt %+v:\n%s\nwant %q", c.attempt, text, c.want)
		}
		if strings.Contains(text, "log in") || strings.Contains(text, "/login") {
			t.Errorf("attempt %+v sends the person to a login:\n%s", c.attempt, text)
		}
	}
	// Over figures it already has, a failed read is a caption beside them.
	obs := &accountstate.Observation{ObservedAt: renderNow - 600, Source: accountstate.SourceStore}
	text := boardText(p.Board([]accountstate.Facts{agyFacts(accountstate.Attempt{State: accountstate.AttemptVendorFailed, ExitCode: 1}, obs)}, renderNow, LayoutCompact, 0))
	if !strings.Contains(text, "agy exited 1") || !strings.Contains(text, "observed 10m ago") {
		t.Errorf("caption over old figures:\n%s", text)
	}
}

// No account on Antigravity's page is one a launch takes, so its compact
// board carries no legend for the mark.
func TestAgyCompactBoardHasNoLaunchLegend(t *testing.T) {
	p := NewPalette(false)
	b := p.Board([]accountstate.Facts{agyFacts(accountstate.Attempt{}, nil)}, renderNow, LayoutCompact, 0)
	if text := boardText(b); strings.Contains(text, "next launch") {
		t.Errorf("legend on Antigravity's page:\n%s", text)
	}
	b = p.Board([]accountstate.Facts{{Vendor: config.Claude, Label: "a@x.com", Health: accountstate.HealthOK}}, renderNow, LayoutCompact, 0)
	if text := boardText(b); !strings.Contains(text, "next launch") {
		t.Errorf("no legend on Claude Code's page:\n%s", text)
	}
}
