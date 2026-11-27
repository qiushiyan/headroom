package check

import (
	"fmt"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// checkOwnState audits state.json — the one file headroom writes for itself.
//
// Every failure here degrades silently by design, so that the surfaces keep
// working: an unreadable section reads as empty, a future timestamp is
// ignored, a stale record is skipped. That design is only honest if something
// says so out loud, and this is it. `own` and `check` are separate on purpose:
// a section headroom could not write is headroom's problem, while a stored
// response that no longer parses is the vendor's, and the closing summary must
// not confuse the two.
func checkOwnState(snap state.Snapshot, r *report) {
	if snap.ReadOnly() {
		// Not a failure: a newer headroom wrote it, and this binary correctly
		// refuses to rewrite what it cannot fully understand.
		r.skip(fmt.Sprintf("state: schema %d understood", snap.Version()),
			fmt.Sprintf("written by a newer headroom (this one writes %d) — it is being read, never written",
				state.Version))
		return
	}
	problems := snap.Problems()
	for _, p := range problems {
		r.own(false, fmt.Sprintf("state[%s]: section readable", p.Section), p.Detail)
	}
	if len(problems) == 0 {
		r.ok("state: every section readable")
	}
	r.own(snap.OwnersReadable(), "state[sessions]: re-home records readable",
		"explicit re-homes are being ignored and cannot be rewritten")
	auditResponses(snap, r)
}

// auditResponses re-reads every stored usage response.
//
// Deliberately not asserted: records naming an account the filesystem no
// longer has. They are how the ledger looks between an account being removed
// and the record ageing out, they make headroom more conservative rather than
// less, and FAIL is reserved for an assumption that was tested and
// contradicted.
func auditResponses(snap state.Snapshot, r *report) {
	now := time.Now()
	for _, rec := range snap.Audit() {
		if rec.FetchedAtMS > now.Add(2*time.Minute).UnixMilli() {
			r.own(false, fmt.Sprintf("state[%s]: observation is not stamped in the future", rec.Name),
				"clock anomaly — these figures are being ignored, so the board shows older ones")
		}
		if len(rec.Body) == 0 {
			continue
		}
		// The strongest drift assertion available, and free: this body came
		// from the endpoint, headroom stored it verbatim, and it is re-parsed
		// here by the function rendering uses. A cached copy of Claude Code's
		// is second-hand evidence next to it.
		label := fmt.Sprintf("state[%s]: stored usage response parses via shared contract", rec.Name)
		reading, err := usage.Parse(snap.Vendor(), rec.Body)
		r.check(err == nil && !reading.Drifted(), label,
			"the response headroom itself stored no longer parses — shape drifted")
	}
}

// checkPeers says how the newest pull from each machine the scope names
// went. It asserts nothing about the vendor or about headroom's files: a
// laptop asleep is not a fault, and the readings it last gave stand, aged.
// It is here so that a machine misnamed in HEADROOM_PEERS, or one running a
// headroom without the readings command, is said somewhere other than in a
// caption that never says "via" it.
func checkPeers(scope config.Scope, snap state.Snapshot, r *report) {
	now := time.Now()
	ago := func(ms int64) string {
		if a := render.Age((now.UnixMilli() - ms) / 1000); a != "just now" {
			return a + " ago"
		}
		return "just now"
	}
	for _, peer := range scope.Peers {
		label := fmt.Sprintf("peer[%s]", peer)
		st, ok := snap.PeerStatus(peer)
		if ok && st.Error == "" && st.PulledMS > 0 {
			r.ok(fmt.Sprintf("%s: answered %s", label, ago(st.PulledMS)))
			continue
		}
		// A note carries no group prefix of its own, and a Codex line must
		// still say it is Codex's.
		label = r.prefix + label
		switch {
		case !ok:
			r.note(label + ": not asked yet — the board, headroom --json and headroom refresh pull from it")
		case st.Error != "":
			newest := "none yet"
			if st.PulledMS > 0 {
				newest = ago(st.PulledMS)
			}
			r.note(fmt.Sprintf("%s: the ask %s failed — %s; its newest answer: %s",
				label, ago(st.AttemptMS), st.Error, newest))
		default:
			r.note(label + ": asked, and its answer is still on its way")
		}
	}
}
