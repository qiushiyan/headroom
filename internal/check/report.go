package check

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// report is one run's lines and its verdict. Every assertion goes through it,
// so who a failure is against is recorded when the line is printed: a vendor
// failure against the group's vendor, an own failure against headroom's own
// files and no vendor at all. The closing line is read off those counts.
type report struct {
	out io.Writer
	p   render.Palette

	// vendor is the group being reported; prefix marks every line of a
	// vendor's group other than Claude Code's, whose lines stay exactly as
	// they were.
	vendor config.Vendor
	prefix string

	fails, ownFails, unknowns int
	drifted                   map[config.Vendor]int // vendor failures, by the vendor they contradict
}

func newReport(out io.Writer, color bool) *report {
	return &report{out: out, p: render.NewPalette(color), vendor: config.Claude, drifted: map[config.Vendor]int{}}
}

// group starts a vendor's lines: what follows is reported against v, each
// line marked with prefix.
func (r *report) group(v config.Vendor, prefix string) { r.vendor, r.prefix = v, prefix }

// ok prints an assertion that held, or a fact that is reported and never fails.
func (r *report) ok(label string) {
	fmt.Fprintf(r.out, "%s ok %s  %s\n", r.p.Grn, r.p.Rst, r.prefix+label)
}

// fail prints an assertion about the vendor that its evidence contradicted:
// the vendor most likely changed a format.
func (r *report) fail(label, hint string) {
	r.failed(label, hint)
	r.drifted[r.vendor]++
}

// check is ok when the assertion held and fail when it did not.
func (r *report) check(ok bool, label, hint string) {
	if ok {
		r.ok(label)
		return
	}
	r.fail(label, hint)
}

// own is check for headroom's own files. A FAIL here means *this tool* wrote
// or lost something, not that the vendor changed a format — and the closing
// line says which, because reporting vendor drift for a corrupt state file
// would confirm a false diagnosis at exactly the moment someone reaches for
// this command.
func (r *report) own(ok bool, label, hint string) {
	if ok {
		r.ok(label)
		return
	}
	r.failed(label, hint)
	r.ownFails++
}

func (r *report) failed(label, hint string) {
	if hint != "" {
		hint = " — " + hint
	}
	fmt.Fprintf(r.out, "%sFAIL%s  %s%s\n", r.p.Red, r.p.Rst, r.prefix+label, hint)
	r.fails++
}

// skip records a fact that could not be tested. It never fails the run.
func (r *report) skip(label, why string) {
	if why != "" {
		why = " — " + why
	}
	fmt.Fprintf(r.out, "%s ?? %s  %s%s\n", r.p.Yel, r.p.Rst, r.prefix+label, why)
	r.unknowns++
}

// note prints a line that asserts nothing and counts toward nothing.
func (r *report) note(line string) {
	fmt.Fprintf(r.out, "%s -- %s  %s\n", r.p.Dim, r.p.Rst, line)
}

// verdict prints the closing line and returns the exit code: FAIL over
// INCONCLUSIVE over PASS, and a failure blamed on whoever it was against.
func (r *report) verdict() int {
	fmt.Fprintln(r.out)
	switch {
	case r.fails > 0 && r.fails == r.ownFails:
		fmt.Fprintf(r.out, "%d check(s) failed, all against headroom's own files — "+
			"nothing here says Claude Code changed anything\n", r.fails)
		return ExitFail
	case r.fails > 0:
		who := "Claude Code"
		switch {
		case r.drifted[config.Codex] > 0 && r.drifted[config.Claude] > 0:
			who = "Claude Code and Codex"
		case r.drifted[config.Codex] > 0:
			who = "Codex"
		}
		fmt.Fprintf(r.out, "%d check(s) failed — %s likely changed a format\n", r.fails, who)
		return ExitFail
	case r.unknowns > 0:
		fmt.Fprintf(r.out, "no assumption broke, but %d could not be tested — "+
			"nothing here says anything is wrong\n", r.unknowns)
		return ExitInconclusive
	default:
		fmt.Fprintln(r.out, "all checks passed")
		return ExitPass
	}
}

// display is the name an account's lines carry: "primary" for the primary,
// whatever it is called, and the account's name otherwise.
func display(a accounts.Account) string {
	if a.IsPrimary() {
		return "primary"
	}
	return a.Name
}

// request is diagnostic policy over the same request evidence the board
// uses, for the group's vendor. Bookkeeping failures are reported first and
// unconditionally — they are independent of whatever the endpoint said — and
// the vendor only decides what a 401 means.
func (r *report) request(name string, res refresh.Result, now time.Time) {
	status := "n/a"
	if res.Attempt.HTTPCode != 0 {
		status = fmt.Sprint(res.Attempt.HTTPCode)
	}
	label := fmt.Sprintf("api[%s]: HTTP %s", name, status)
	if res.StoreErr != nil {
		bookkeeping := fmt.Sprintf("state[%s]: request bookkeeping", name)
		if errors.Is(res.StoreErr, state.ErrBusy) || errors.Is(res.StoreErr, state.ErrReadOnly) {
			r.skip(bookkeeping, res.StoreErr.Error())
		} else {
			r.own(false, bookkeeping, res.StoreErr.Error())
		}
	}
	switch res.Attempt.State {
	case accountstate.AttemptStateUnavailable:
		r.skip(fmt.Sprintf("api[%s]: not tested", name), "headroom could not authorize a request against its state file")
	case accountstate.AttemptDeferred:
		r.skip(fmt.Sprintf("api[%s]: not tested", name), fmt.Sprintf("inside this account's quiet period — %s remaining", time.Unix(res.Attempt.NextEligibleAt, 0).Sub(now).Round(time.Second)))
	case accountstate.AttemptTransport:
		r.skip(label, "transport error — no evidence either way")
	case accountstate.AttemptRefused:
		r.skip(label, "rate limited — no evidence either way")
	case accountstate.AttemptUnparseable:
		r.fail(label+", unparseable body", "shape drifted")
	case accountstate.AttemptHTTP:
		switch {
		case res.Attempt.HTTPCode >= 500:
			r.skip(label, "vendor-side error — no evidence either way")
		case res.Attempt.HTTPCode == http.StatusUnauthorized && r.vendor == config.Codex && res.TokenAfter401 != refresh.TokenChanged:
			// Codex recovers from a 401 by reloading and refreshing, so a
			// rejected token is never evidence that anything drifted — where
			// Claude Code's unchanged-token 401 below is.
			r.skip(label, "access token rejected — any Codex session refreshes it; no evidence either way")
		case res.Attempt.HTTPCode == http.StatusUnauthorized && res.TokenAfter401 == refresh.TokenChanged:
			r.skip(label, "token was refreshed mid-check — no evidence either way")
		case res.Attempt.HTTPCode == http.StatusUnauthorized && res.TokenAfter401 == refresh.TokenUnknown:
			r.skip(label, "credential unreadable on re-check — cannot tell a refresh race from drift")
		default:
			r.fail(label, "unexpected status — endpoint or auth drifted")
		}
	case accountstate.AttemptOK, accountstate.AttemptNoLimits:
		bad := 0
		for _, row := range res.Observation.Rows {
			if row.Drifted() {
				bad++
			}
		}
		if res.Observation.Allowance.State == usage.AllowanceBad {
			bad++
		}
		hint := ""
		if bad > 0 {
			hint = fmt.Sprintf("%d malformed field(s) — shape drifted", bad)
		}
		r.check(bad == 0, fmt.Sprintf("%s, %d row(s), fields well-formed", label, len(res.Observation.Rows)), hint)
	}
}
