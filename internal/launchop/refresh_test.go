package launchop

import (
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// A launch on a machine whose own tokens have all aged out still leaves a
// refresh when a pull from another machine is due: that pull is the only way
// its figures move. Inside the pull's spacing, and with no peers, it leaves
// none.
func TestALaunchRefreshesForADuePull(t *testing.T) {
	now := time.Now()
	scope := config.Scope{Vendor: config.Claude, AccountsRoot: t.TempDir(), Spacing: config.DefaultSpacing, Peers: []string{"mini2"}}
	st := state.Open(scope)
	f := func() facts { return facts{set: accounts.Set{Scope: scope}, snap: st.Load()} }
	if !worthRefreshing(f(), now) {
		t.Error("no refresh with a pull due and nothing else to ask")
	}
	st.ClaimPull("mini2", now)
	if worthRefreshing(f(), now.Add(time.Second)) {
		t.Error("a refresh inside the pull's spacing, with nothing else to ask")
	}
	scope.Peers = nil
	st = state.Open(scope)
	if worthRefreshing(f(), now) {
		t.Error("a refresh with no peers and nothing to ask")
	}
}
