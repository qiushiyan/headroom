package check

// The Antigravity group: the facts headroom stands on to show agy's quota,
// verified through the same candidate, run and parser the board uses
// (refresh.PrepareAgy, usage.Parse).
//
// It differs from the other groups in what it can prove. There is no login
// to inspect — headroom reads nothing of agy's sign-in — and no binary seam
// to search, since Homebrew puts a wrapper script on PATH. The one read is
// the evidence: it answers in the shape headroom parses, and its log names
// whose sign-in answered.

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/state"
)

func checkAgy(scope config.Scope, environ []string, r *report) {
	if _, err := exec.LookPath(scope.Binary()); err != nil {
		r.fail("binary: agy on PATH", fmt.Sprintf("not found, though %s exists", scope.PrimaryDir()))
		return
	}
	r.ok("binary: agy on PATH")

	// The variables a read strips are said, as a launch's are: agy run by
	// anything else in this shell still obeys them.
	for _, in := range launch.Redirects(config.Agy, environ) {
		r.ok(fmt.Sprintf("env: inherited %s is stripped from headroom's reads", in.String()))
	}

	set := accounts.Discover(scope)
	a := set.Accounts[0]
	candidate, _ := refresh.PrepareAgy(a, environ)
	st := state.Open(scope)
	var res refresh.Result
	for got := range refresh.Start(context.Background(), st, []*refresh.Candidate{candidate}, nil) {
		res = got
	}
	r.request(display(a), res, time.Now())
	if res.Attempt.State.Answered() && res.Observation != nil {
		r.check(res.Observation.Email != "",
			fmt.Sprintf("identity[%s]: the read's log names its sign-in", display(a)),
			"no applyAuthResult line — the board cannot say whose figures these are")
	}
	checkOwnState(st.Load(), r)
}
