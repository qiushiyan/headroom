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

	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
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

// refreshWorthStarting reports whether any account could be asked right now:
// its quiet period has passed and its stored token is usable. The claim still
// decides; this only keeps a launch from spawning a process that could ask
// nobody — which, on a machine whose idle accounts' tokens have aged out,
// would otherwise be every launch. Whether a launch leaves a refresh at all is
// the launch operation's to decide (a choice the rule made); this is only
// whether one would find anybody to ask.
func refreshWorthStarting(f placeFacts, now time.Time) bool {
	for _, c := range f.cands {
		if c.askable(now) && !f.snap.NextEligible(c.key, now).After(now) {
			return true
		}
	}
	return false
}

// askable reports whether the account's stored login could carry a usage
// request right now. For Claude Code that is the credential the placement
// read, which an unread or unreadable one never is; Codex's is the auth.json
// snapshot discovery took.
func (c placeCandidate) askable(now time.Time) bool {
	a := c.acct
	if a.Scope.Vendor == config.Codex {
		return a.Auth.State == codexauth.OK && !a.Auth.TokenStale(now.Unix())
	}
	blob, ok := creds.Parse(c.cred)
	return ok && a.Readable && blob.TokenUsable(now.UnixMilli())
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
			st := state.Open(scope)
			p := prepare(ctx, scope, st, false)
			for range launchFetches(ctx, p.list, st) {
			}
		}(scope)
	}
	wg.Wait()
	return 0
}
