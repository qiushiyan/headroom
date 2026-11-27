package check

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// Each named machine gets a line: answered, failed and why, or never asked.
// None of them changes the verdict — a laptop asleep is not a fault — and a
// Codex line says it is Codex's.
func TestCheckSaysHowEachMachineAnswered(t *testing.T) {
	scope := config.Scope{Vendor: config.Codex, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing,
		Peers: []string{"mini2", "laptop", "desk"}}
	st := state.Open(scope)
	now := time.Now()
	st.ClaimPull("mini2", now)
	st.RecordPull("mini2", nil, "", now)
	st.ClaimPull("laptop", now)
	st.RecordPull("laptop", nil, "ssh could not reach it: timed out", now)

	var out bytes.Buffer
	r := newReport(&out, false)
	r.group(config.Codex, "codex ")
	checkPeers(scope, st.Load(), r)
	got := out.String()
	for _, want := range []string{
		"codex peer[mini2]: answered",
		"codex peer[laptop]: the ask just now failed — ssh could not reach it: timed out; its newest answer: none yet",
		"codex peer[desk]: not asked yet",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("check lacks %q:\n%s", want, got)
		}
	}
	if r.fails != 0 || r.unknowns != 0 {
		t.Errorf("peer lines changed the verdict: %d fails, %d unknowns", r.fails, r.unknowns)
	}
}
