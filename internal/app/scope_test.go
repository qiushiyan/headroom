package app

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
)

// claudeScope is the Claude Code scope under a fixture home, with the
// primary's name pinned the way the fixtures expect it.
func claudeScope(home, primary string) config.Scope {
	s := config.ForHome(home).Claude
	s.PrimaryName = primary
	return s
}

func withUsageURL(s config.Scope, url string) config.Scope {
	s.UsageURL = url
	return s
}

// claudeBoard wraps accounts the way a Claude Code-only run reports them.
func claudeBoard(accts []accountstate.Account, current string) []round.View {
	return []round.View{{Scope: config.Scope{Vendor: config.Claude}, Accounts: accts, Routing: accounts.Routing{Current: current}}}
}

// preparedWith is a round's local half with its credentials and health
// injected: no Keychain, no Claude Code install.
func preparedWith(cfg config.Scope, st *state.Store, now time.Time, blobs map[string]string, health map[string]auth.Status) *round.Round {
	return round.Prepare(context.Background(), cfg, st, now, round.Deps{
		Creds:  func(_ context.Context, a accounts.Account) string { return blobs[a.ConfigDir] },
		Health: func([]accounts.Account) auth.QueryFunc { return func(dir string) auth.Status { return health[dir] } },
	})
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// byName indexes accounts by name.
func byName(accts []accountstate.Account) map[string]accountstate.Account {
	out := map[string]accountstate.Account{}
	for _, d := range accts {
		out[d.Acct.Name] = d
	}
	return out
}
