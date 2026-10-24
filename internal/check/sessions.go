package check

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tag"
)

// checkSessionStore verifies the contracts the session picker stands on:
// prompt history attributes sessions to accounts, transcripts carry their own
// titles and a verifiable cwd within the tail budget, the live-session
// registry parses (it is what stops `dd` from deleting an open transcript),
// and headroom's saved re-homes are readable. Shapes, never census numbers —
// counts are wrong the day after they're written down.
func checkSessionStore(cfg config.Scope, accts []accounts.Account, snap state.Snapshot, probe sessions.PIDProbe, r *report) {
	// history.jsonl: the attribution source. Absent or empty is a fresh
	// account, not drift; present with lines but zero parseable claims means
	// the shape moved and affinity is silently routing everything to the
	// current account.
	for _, a := range accts {
		f, err := os.Open(filepath.Join(a.Dir(), "history.jsonl"))
		if err != nil {
			continue
		}
		h := sessions.ParseHistory(f)
		f.Close()
		if h.Lines == 0 {
			continue
		}
		r.check(len(h.Newest) > 0,
			fmt.Sprintf("history[%s]: prompt records carry sessionId + timestamp", display(a)),
			"no line parses — session→account attribution is gone")
	}

	// The live-session registry: when claim files exist, they must parse,
	// or every open session silently reads as deletable.
	regFiles, regProblems := 0, 0
	var claims []sessions.RegistryEntry
	for _, a := range accts {
		reg := sessions.ReadRegistry(a.Name, a.Dir())
		regFiles += len(reg.Entries)
		regProblems += len(reg.Problems)
		claims = append(claims, reg.Entries...)
	}
	// Live by the one definition placement itself uses: the pid runs and its
	// start matches the claim. A stale record whose pid another process has
	// since taken is not a running session, and must not be judged as one.
	_, running := sessions.InspectClaims(claims, probe)
	if regFiles == 0 && regProblems == 0 {
		r.skip("registry: not tested", "no live-session records right now")
	} else {
		r.check(regProblems == 0, "registry: live-session records carry sessionId + pid + startedAt",
			fmt.Sprintf("%d registry read problems — liveness is incomplete", regProblems))
	}
	checkRegistryStatus(running, r)

	// Transcripts, through the same collector the picker uses. Per-file
	// absence of a title is ordinary (print-mode sessions); a store where
	// *nothing* titles or *nothing* cwd-verifies means the record shapes
	// drifted.
	listing := sessions.Collect(sessions.Input{
		ProjectsDir: cfg.StoreDir(),
		CWD:         cfg.Home,
		Owners:      snap.Owners(),
	})
	if len(listing.Sessions) == 0 {
		r.skip("sessions: not tested", "store is empty")
		return
	}
	titled, cwdOK := 0, 0
	for _, s := range listing.Sessions {
		if s.Tail.Title() != "" {
			titled++
		}
		if s.CWD != "" {
			cwdOK++
		}
	}
	r.check(titled > 0, "sessions: title/prompt records parse via shared contract",
		"no transcript yields a title — record shapes drifted")
	r.check(cwdOK > 0, "sessions: cwd records verify against store dir names",
		"no transcript's cwd munges to its dir — resume targets are gone")

	// The tail budget is the assertion most likely to actually fire, and
	// it must test the whole claim: for every transcript larger than the
	// budget, the tail-derived title *and* cwd must equal what a
	// full-file parse resolves — a custom rename beyond the budget with
	// a newer last-prompt inside it renders the wrong title while "some
	// title exists" still passes. The comparison runs through the shared
	// parser, never a substring scan: transcripts about these tools
	// quote the record types verbatim in message bodies, and a raw
	// needle match reads that as a title (it did, on this very repo's
	// own sessions).
	over := 0
	for _, s := range listing.Sessions {
		if s.Size <= sessions.TailBudget {
			continue // the tail pass already saw the whole file
		}
		data, err := os.ReadFile(s.Path)
		if err != nil {
			continue
		}
		full := sessions.ParseTail(data, filepath.Base(s.StoreDir), true)
		if full.Title() != s.Tail.Title() || full.CWD != s.CWD || full.Model != s.Tail.Model {
			over++
		}
	}
	r.check(over == 0, "sessions: the adaptive tail reader resolves the same title/cwd/model as a full parse",
		fmt.Sprintf("%d transcript(s) resolve differently — a record drifted out of the reader's reach", over))
}

// checkRegistryStatus verifies the one registry field automatic placement
// reads beyond liveness: the vendor's word for what a session is doing. Only
// verified-live claims are judged — a record left by a crash, or one whose pid
// has been recycled, says nothing about what the vendor writes today.
//
// When no live session carries a status string the field is gone, and that
// fails: busy sessions would silently stop counting as load. When only some
// lack one — sessions started under a build from before the field, still
// running beside newer ones — that is reported as untested rather than as
// drift. A status this binary has not seen is not drift either: the
// vocabulary is the vendor's and open, the assumption could not be tested for
// that record, and such a session counts as not busy.
func checkRegistryStatus(running []sessions.RegistryEntry, r *report) {
	if len(running) == 0 {
		return
	}
	missing := 0
	unknown := map[string]bool{}
	for _, e := range running {
		switch {
		case e.StatusState != tag.OK:
			missing++
		case !knownStatus[e.Status]:
			unknown[e.Status] = true
		}
	}
	label := "registry: running sessions say what they are doing (status)"
	switch {
	case missing == len(running):
		r.fail(label, "none carries a status string — busy sessions are no longer counted as load")
	case missing > 0:
		r.skip(label, fmt.Sprintf("%d of %d carry no status string — sessions from an older build, or the field is going away", missing, len(running)))
	default:
		r.ok(label)
	}
	if len(unknown) > 0 {
		words := make([]string, 0, len(unknown))
		for w := range unknown {
			words = append(words, strconv.Quote(w))
		}
		sort.Strings(words)
		r.skip("registry: every status is one this binary knows",
			"unfamiliar status "+strings.Join(words, ", ")+" — such a session counts as not busy")
	}
}

// knownStatus is the registry status vocabulary observed so far (2.1.286).
// Only "busy" is given a meaning; the others are listed so a new word is
// noticed.
var knownStatus = map[string]bool{sessions.StatusBusy: true, "idle": true, "shell": true}
