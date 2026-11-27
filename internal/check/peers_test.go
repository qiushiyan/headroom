package check

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// Each named machine gets its lines: answered; failed, under the failure's
// own time; asked with no outcome yet; or never asked. None of them changes the verdict — a laptop asleep is not a fault —
// and a Codex line says it is Codex's.
func TestCheckSaysHowEachMachineAnswered(t *testing.T) {
	scope := config.Scope{Vendor: config.Codex, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing,
		Peers: []string{"mini2", "laptop", "desk"}}
	st := state.Open(scope)
	now, then := time.Now(), time.Now().Add(-time.Hour)
	c, _ := st.ClaimPull("mini2", now)
	st.RecordPull("mini2", c.Generation, map[string]state.Observation{"uuid:other": {FetchedAtMS: now.UnixMilli(), Body: []byte(`{}`)}}, "", now)
	c, _ = st.ClaimPull("laptop", then)
	st.RecordPull("laptop", c.Generation, nil, "ssh could not reach it: timed out", then)
	st.ClaimPull("laptop", now) // cut short: no outcome recorded

	var out bytes.Buffer
	r := newReport(&out, false)
	r.group(config.Codex, "codex ")
	checkPeers(scope, accounts.Set{Scope: scope, Accounts: []accounts.Account{{Name: "a@x.com", AccountID: "a"}}}, st.Load(), r)
	got := out.String()
	for _, want := range []string{
		"codex peer[mini2]: answered just now",
		"codex peer[laptop]: the ask 1h ago failed — ssh could not reach it: timed out; its newest answer: none yet",
		"codex peer[laptop]: asked just now, with no outcome recorded",
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

// Both vendors' groups say how their pulls stand, through the run itself,
// without changing its verdict; a name that is no ssh destination fails the
// run against headroom's own configuration, never against a vendor.
func TestRunSaysHowPullsStand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"limits":[]}`)) }))
	defer srv.Close()
	home := t.TempDir()
	claude, _ := soundClaudeTree(t, home)
	claude.UsageURL = srv.URL
	claude.Peers = []string{"mini2"}
	codex := config.ForHome(home).Codex
	codex.Present, codex.Peers = true, []string{"mini2"}
	cfg := config.Config{Claude: claude, Codex: codex}

	var out strings.Builder
	code := Run(cfg, &out, false, nil)
	text := out.String()
	if !strings.Contains(text, " peer[mini2]: not asked yet") || !strings.Contains(text, "codex peer[mini2]: not asked yet") {
		t.Errorf("a group left out its peer line:\n%s", text)
	}
	if strings.Contains(text, "FAIL  peer") || strings.Contains(text, "FAIL  codex peer") {
		t.Errorf("a peer line failed the run:\n%s", text)
	}

	// Claude Code's group alone, so the closing line answers for this line.
	cfg.Codex.Present = false
	cfg.BadPeers = []string{"-oProxyCommand=x"}
	out.Reset()
	code = Run(cfg, &out, false, nil)
	text = out.String()
	if code != ExitFail || !strings.Contains(text, `FAIL  config: HEADROOM_PEERS names "-oProxyCommand=x"`) || !strings.Contains(text, "all against headroom's own files") {
		t.Errorf("a bad name: exit %d\n%s", code, text)
	}
}
