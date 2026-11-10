package check

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/tag"
)

// checkSessionStore verifies the vendor records a launch reads about
// sessions: prompt history attributes a session to the account that drove
// it, and the live-session registry parses (it is busy load, and it is what
// keeps `accounts remove` from deleting an account a session runs on).
// Shapes, never census numbers — counts are wrong the day after they're
// written down.
func checkSessionStore(accts []accounts.Account, probe sessions.PIDProbe, r *report) {
	// history.jsonl: the attribution source. Absent or empty is a fresh
	// account, not drift; present with lines but zero parseable claims means
	// the shape moved and affinity is silently routing everything to the
	// current account.
	for _, a := range accts {
		h, ok := sessions.ReadHistory(a.Dir())
		if !ok || h.Lines == 0 {
			continue
		}
		r.check(len(h.Newest) > 0,
			fmt.Sprintf("history[%s]: prompt records carry sessionId + timestamp", display(a)),
			"no line parses — session→account attribution is gone")
	}

	// The live-session registry: when claim files exist, they must parse,
	// or every open session silently reads as idle.
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
