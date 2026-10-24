package app

// The detached refresh: the round an automatic launch leaves behind it, which
// is the `headroom refresh` command. The launching process is about to be
// replaced by the vendor and cannot wait on an endpoint, so the figures it
// chose on are refreshed for the next launch by a child that outlives it.

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
)

// startRefresh starts one detached refresh round for the scope's vendor and
// returns at once. The launching process is about to be replaced by the
// vendor, so it cannot wait for a response — and a request whose answer nobody
// stays to record would spend the account's budget for nothing. The child is
// an ordinary headroom running the ordinary round: the claim authorizes, the
// completion records, and it has no terminal to disturb.
var startRefresh = func(scope config.Scope) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "refresh", "--vendor", string(scope.Vendor))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// placementCreds reads one Claude Code account's credential blob for a
// placement.
var placementCreds = func(a accounts.Account) string {
	raw, _ := creds.ReadRaw(context.Background(), a.ConfigDir, a.Dir())
	return raw
}

// placeDeps are the edges a placement reads through, as this process has
// them.
func placeDeps() launchop.Deps {
	return launchop.Deps{Creds: placementCreds, Probe: pidProbe, Refresh: startRefresh}
}

// refreshDeadline bounds the round a launch leaves behind: the claim, and a
// fetch the HTTP client already gives up on after ten seconds. Completion
// follows it either way. The round lives inside whatever launched it — an
// automated job that is torn down when it ends counts it as its own process —
// so it must be short, and must stop cleanly when told to.
const refreshDeadline = 12 * time.Second

// runRefresh is the round with nobody watching: ask about every account the
// claim permits, record the answers, print nothing. It is what an automatic
// launch leaves running behind it.
//
// SIGTERM, SIGINT and SIGHUP end it early and cleanly, whatever it is doing:
// a credential read in progress is killed, requests in flight are abandoned
// and recorded as abandoned, an answer that already arrived is kept, and
// nothing is claimed once the signal has come. It is then gone within
// about a second — the lock a completion takes is held for milliseconds — so
// a supervisor's TERM-then-KILL never has to reach the KILL. Killed outright
// mid-request, it leaves a claim that simply expires at its spacing: a claim
// is a reservation with a deadline, which no reader waits on, and the lock
// dies with the process.
func runRefresh(scopes []config.Scope) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, refreshDeadline)
	defer cancel()
	var wg sync.WaitGroup
	for _, scope := range scopes {
		wg.Add(1)
		go func(scope config.Scope) {
			defer wg.Done()
			round.Prepare(ctx, scope, state.Open(scope), time.Now(), roundDeps(false)).Complete(ctx)
		}(scope)
	}
	wg.Wait()
	return 0
}
