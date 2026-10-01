// Package check verifies the assumptions headroom reverse-engineered from
// Claude Code. Each FAIL means Claude Code likely changed a format on
// update. Checks go through the exact same parsers rendering uses, so the
// checker cannot drift from what rendering actually needs.
package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/refresh"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

// Exit codes: a checker that cannot distinguish "the assumption broke" from
// "I couldn't test it" is worse than useless when the dashboard misbehaves —
// it confirms the false diagnosis. Rate limiting, a dead network and a token
// mid-refresh are all reasons no evidence was gathered, not evidence against.
const (
	ExitPass         = 0
	ExitFail         = 1
	ExitInconclusive = 2
)

// probe is the process inspection that proves a registry claim live — a pid
// that runs *and* started when the claim says. It is injected because exec
// stays at the edges; nil means liveness cannot be established, and the
// assertions that need a live session are then not made.
func Run(cfg config.Config, out io.Writer, color bool, probe sessions.PIDProbe) int {
	scope := cfg.Claude
	p := render.NewPalette(color)
	fails, unknowns, ownFails := 0, 0, 0
	// prefix marks every line of a vendor's group other than Claude Code's,
	// whose lines stay exactly as they were; drifted records which vendor a
	// non-own failure was against, so the closing line blames the right one.
	prefix, vendor := "", config.Claude
	drifted := map[config.Vendor]int{}

	chk := func(ok bool, label, hint string) {
		label = prefix + label
		if ok {
			fmt.Fprintf(out, "%s ok %s  %s\n", p.Grn, p.Rst, label)
			return
		}
		if hint != "" {
			hint = " — " + hint
		}
		fmt.Fprintf(out, "%sFAIL%s  %s%s\n", p.Red, p.Rst, label, hint)
		fails++
		drifted[vendor]++
	}
	// own is chk for headroom's own files. A FAIL here means *this tool*
	// wrote or lost something, not that Claude Code changed a format — and
	// the closing line says which, because reporting vendor drift for a
	// corrupt state file would confirm a false diagnosis at exactly the
	// moment someone reaches for this command.
	own := func(ok bool, label, hint string) {
		before := fails
		chk(ok, label, hint)
		if fails > before {
			ownFails++
			drifted[vendor]--
		}
	}
	// skip records a fact that could not be tested. It never fails the run.
	skip := func(label, why string) {
		if why != "" {
			why = " — " + why
		}
		fmt.Fprintf(out, "%s ?? %s  %s%s\n", p.Yel, p.Rst, prefix+label, why)
		unknowns++
	}

	// The installed binary still carries the seams this tooling depends on:
	// the usage endpoint, config-dir routing, and the credentials service
	// name.
	needles := []string{"api/oauth/usage", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "-credentials"}
	bin := claudeBinary()
	binName, missing := "", []string(nil)
	if bin != "" {
		binName = filepath.Base(bin)
		found := searchFile(bin, needles)
		for _, n := range needles {
			if !found[n] {
				missing = append(missing, n)
			}
		}
	}
	hint := "missing: binary not found"
	if bin != "" && len(missing) > 0 {
		hint = "missing: " + strings.Join(missing, " ")
	}
	chk(bin != "" && len(missing) == 0,
		fmt.Sprintf("binary (%s): endpoint + config-dir + credential seams present", binName), hint)

	// Resolve credentials exactly as the board does, and parse the selected
	// blob through its shared contract. Fetches run in parallel and are
	// validated after the join, so a dead network costs one timeout.
	set := accounts.Discover(scope)
	accts := set.Accounts
	now := time.Now()
	st := state.Open(scope)
	requests := make([]*refresh.Candidate, len(accts))
	for i, a := range accts {
		name := a.Name
		if a.IsPrimary() {
			name = "primary"
		} else if _, err := os.Stat(a.MetaPath()); err != nil {
			continue
		}
		raw, source := creds.ReadRaw(a.ConfigDir, a.Dir())
		blob, ok := creds.Parse(raw)
		label := fmt.Sprintf("credential[%s]: %s", name, credentialSourceLabel(source))
		hint := "credential blob did not parse (accessToken missing or format changed)"
		if source == creds.SourceNone {
			hint = "not logged in — no Keychain item or .credentials.json"
		}
		chk(ok, label, hint)
		candidate, eligibility := refresh.Prepare(a, blob, ok, now)
		requests[i] = candidate
		if candidate == nil {
			reason := "credential blob did not parse"
			switch eligibility {
			case accountstate.AttemptTokenStale:
				reason = fmt.Sprintf("access token stale — any %s session refreshes it", accounts.Launcher(a))
			case accountstate.AttemptIdentityUnknown:
				reason = "account identity unreadable — request budget cannot be identified"
			}
			skip(fmt.Sprintf("api[%s]: not tested", name), reason)
		}
	}
	results := make([]refresh.Result, len(accts))
	for r := range refresh.Start(context.Background(), st, requests, rereadCredential(scope.PrimaryDir())) {
		results[r.Index] = r
	}
	for i, candidate := range requests {
		if candidate == nil {
			continue
		}
		name := accts[i].Name
		if accts[i].IsPrimary() {
			name = "primary"
		}
		reportRequest(config.Claude, name, results[i], now, chk, own, skip)
	}

	// .claude.json still records the logged-in email (dashboard labels).
	_, ok := accounts.MetaEmail(scope.PrimaryMeta())
	chk(ok, "claude.json: .oauthAccount.emailAddress present", "")

	// The two surfaces this tool grew to depend on are checked through the
	// same parsers rendering uses, or the checker would pass while the
	// dashboard silently fell back to guesses.
	for _, a := range accts {
		name := "primary"
		if !a.IsPrimary() {
			name = a.Name
		}
		if !a.IsPrimary() {
			if _, err := os.Stat(a.MetaPath()); err != nil {
				continue
			}
		}
		switch auth.Query(a.ConfigDir).Outcome {
		case auth.OutcomeOK:
			chk(true, fmt.Sprintf("auth[%s]: claude auth status parses via shared contract", name), "")
		case auth.OutcomeUnparseable:
			chk(false, fmt.Sprintf("auth[%s]: claude auth status parses via shared contract", name),
				"output shape drifted — account health can no longer be established")
		case auth.OutcomeUnrunnable:
			// Not the vendor's problem: launch refused to build the probe's
			// environment (a dir it will not hand to CLAUDE_CONFIG_DIR).
			// The dirs-absolute assertion in checkRouting names the dir.
			own(false, fmt.Sprintf("auth[%s]: probe environment could not be constructed", name),
				"this account's config dir was refused by the launch seam")
		default:
			skip(fmt.Sprintf("auth[%s]: not tested", name), "claude auth status unavailable")
		}

		// A cache that is present must be usable. Absent is ordinary — Claude
		// Code writes one only once it has fetched — but present-and-rejected
		// means the offline fallback silently disappeared, which is exactly
		// the drift this check exists to catch.
		cacheLabel := fmt.Sprintf("cache[%s]: Claude Code's cached usage parses via shared contract", name)
		switch a.Meta.CacheState {
		case tag.None:
			continue
		case tag.Bad:
			chk(false, cacheLabel,
				"cached payload present but its account identity or fetch time is missing — fallback lost")
			continue
		}
		rows, err := usage.ParseLimits(a.Meta.CachedUsage)
		nbad := 0
		for _, r := range rows {
			if r.Drifted() {
				nbad++
			}
		}
		chk(err == nil && nbad == 0, cacheLabel,
			"cached payload shape drifted — the offline fallback is unreadable")
	}

	checkOwnState(st.Load(), chk, own, skip)
	// Deliberately outside checkOwnState: these depend on .current and the
	// process environment, not on state.json, so a state document written by
	// a newer headroom (which rightly short-circuits the state audit) must
	// not silence them.
	checkRouting(set, os.Environ(), chk, own)
	checkHomes(set, st.Load(), chk, own)
	checkRetention(set, own, skip)
	checkSessionStore(scope, accts, probe, chk, skip)

	// Codex's group runs after Claude Code's, through the same reporters.
	// `check` takes no --vendor: it runs every present vendor's checks, and an
	// absent Codex is one informational line that changes no exit code.
	if cfg.Codex.Present {
		prefix, vendor = "codex ", config.Codex
		checkCodex(cfg.Codex, os.Environ(), chk, own, skip)
		prefix, vendor = "", config.Claude
	} else {
		fmt.Fprintf(out, "%s -- %s  codex: not found (no %s, no %s) — Codex checks not run\n",
			p.Dim, p.Rst, cfg.Codex.PrimaryDir(), cfg.Codex.AccountsRoot)
	}

	fmt.Fprintln(out)
	switch {
	case fails > 0 && fails == ownFails:
		fmt.Fprintf(out, "%d check(s) failed, all against headroom's own files — "+
			"nothing here says Claude Code changed anything\n", fails)
		return ExitFail
	case fails > 0:
		who := "Claude Code"
		switch {
		case drifted[config.Codex] > 0 && drifted[config.Claude] > 0:
			who = "Claude Code and Codex"
		case drifted[config.Codex] > 0:
			who = "Codex"
		}
		fmt.Fprintf(out, "%d check(s) failed — %s likely changed a format\n", fails, who)
		return ExitFail
	case unknowns > 0:
		fmt.Fprintf(out, "no assumption broke, but %d could not be tested — "+
			"nothing here says anything is wrong\n", unknowns)
		return ExitInconclusive
	default:
		fmt.Fprintln(out, "all checks passed")
		return ExitPass
	}
}

// checkSessionStore verifies the contracts the resume surface stands on:
// prompt history attributes sessions to accounts, transcripts carry their own
// titles and a verifiable cwd within the tail budget, the live-session
// registry parses (it is what stops `dd` from deleting an open transcript),
// and headroom's saved re-homes are readable. Shapes, never census numbers —
// counts are wrong the day after they're written down.
func checkSessionStore(cfg config.Scope, accts []accounts.Account, probe sessions.PIDProbe,
	chk func(bool, string, string), skip func(string, string)) {

	// history.jsonl: the attribution source. Absent or empty is a fresh
	// account, not drift; present with lines but zero parseable claims means
	// the shape moved and affinity is silently routing everything to the
	// current account.
	for _, a := range accts {
		name := "primary"
		if !a.IsPrimary() {
			name = a.Name
		}
		f, err := os.Open(filepath.Join(a.Dir(), "history.jsonl"))
		if err != nil {
			continue
		}
		h := sessions.ParseHistory(f)
		f.Close()
		if h.Lines == 0 {
			continue
		}
		chk(len(h.Newest) > 0,
			fmt.Sprintf("history[%s]: prompt records carry sessionId + timestamp", name),
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
		skip("registry: not tested", "no live-session records right now")
	} else {
		chk(regProblems == 0, "registry: live-session records carry sessionId + pid + startedAt",
			fmt.Sprintf("%d registry read problems — liveness is incomplete", regProblems))
	}
	checkRegistryStatus(running, chk, skip)

	// Transcripts, through the same collector the picker uses. Per-file
	// absence of a title is ordinary (print-mode sessions); a store where
	// *nothing* titles or *nothing* cwd-verifies means the record shapes
	// drifted.
	listing := sessions.Collect(sessions.Input{
		ProjectsDir: cfg.StoreDir(),
		CWD:         cfg.Home,
		Owners:      state.Open(cfg).Load().Owners(),
	})
	if len(listing.Sessions) == 0 {
		skip("sessions: not tested", "store is empty")
	} else {
		titled, cwdOK := 0, 0
		for _, s := range listing.Sessions {
			if s.Tail.Title() != "" {
				titled++
			}
			if s.CWD != "" {
				cwdOK++
			}
		}
		chk(titled > 0, "sessions: title/prompt records parse via shared contract",
			"no transcript yields a title — record shapes drifted")
		chk(cwdOK > 0, "sessions: cwd records verify against store dir names",
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
		chk(over == 0, "sessions: the adaptive tail reader resolves the same title/cwd/model as a full parse",
			fmt.Sprintf("%d transcript(s) resolve differently — a record drifted out of the reader's reach", over))
	}

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
func checkRegistryStatus(running []sessions.RegistryEntry, chk func(bool, string, string), skip func(string, string)) {
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
		chk(false, label, "none carries a status string — busy sessions are no longer counted as load")
	case missing > 0:
		skip(label, fmt.Sprintf("%d of %d carry no status string — sessions from an older build, or the field is going away", missing, len(running)))
	default:
		chk(true, label, "")
	}
	if len(unknown) > 0 {
		words := make([]string, 0, len(unknown))
		for w := range unknown {
			words = append(words, strconv.Quote(w))
		}
		sort.Strings(words)
		skip("registry: every status is one this binary knows",
			"unfamiliar status "+strings.Join(words, ", ")+" — such a session counts as not busy")
	}
}

// knownStatus is the registry status vocabulary observed so far (2.1.286).
// Only "busy" is given a meaning; the others are listed so a new word is
// noticed.
var knownStatus = map[string]bool{sessions.StatusBusy: true, "idle": true, "shell": true}

// checkOwnState audits state.json — the one file headroom writes for itself.
//
// Every failure here degrades silently by design, so that the surfaces keep
// working: an unreadable section reads as empty, a future timestamp is
// ignored, a stale record is skipped. That design is only honest if something
// says so out loud, and this is it. `own` and `chk` are separate on purpose:
// a section headroom could not write is headroom's problem, while a stored
// response that no longer parses is the vendor's, and the closing summary must
// not confuse the two.
func checkOwnState(snap state.Snapshot,
	chk, own func(bool, string, string), skip func(string, string)) {

	for _, doc := range snap.Documents() {
		name := "state"
		if doc.Name != "" {
			name = "state(" + doc.Name + ")"
		}
		if doc.ReadOnly {
			// Not a failure: a newer headroom wrote it, and this binary
			// correctly refuses to rewrite what it cannot fully understand.
			// The other document, if there is one, is still this binary's
			// to judge.
			skip(fmt.Sprintf("%s: schema %d understood", name, doc.Version),
				fmt.Sprintf("written by a newer headroom (this one writes %d) — it is being read, never written",
					state.Version))
			continue
		}
		for _, p := range doc.Problems {
			own(false, fmt.Sprintf("state[%s]: section readable", p.Section), p.Detail)
		}
		if len(doc.Problems) == 0 {
			own(true, name+": every section readable", "")
		}
		if doc.ReHomes {
			own(snap.OwnersReadable(), "state[sessions]: re-home records readable",
				"explicit re-homes are being ignored and cannot be rewritten")
		}
		if doc.Ledger {
			auditResponses(snap, chk, own)
		}
	}
}

// auditResponses re-reads every stored usage response.
//
// Deliberately not asserted: records naming an account the filesystem no
// longer has. They are how the ledger looks between an account being removed
// and the record ageing out, they make headroom more conservative rather than
// less, and FAIL is reserved for an assumption that was tested and
// contradicted.
func auditResponses(snap state.Snapshot, chk, own func(bool, string, string)) {
	now := time.Now()
	for _, r := range snap.Audit() {
		if r.FetchedAtMS > now.Add(2*time.Minute).UnixMilli() {
			own(false, fmt.Sprintf("state[%s]: observation is not stamped in the future", r.Name),
				"clock anomaly — these figures are being ignored, so the board shows older ones")
		}
		if len(r.Body) == 0 {
			continue
		}
		// The strongest drift assertion available, and free: this body came
		// from the endpoint, headroom stored it verbatim, and it is re-parsed
		// here by the function rendering uses. A cached copy of Claude Code's
		// is second-hand evidence next to it.
		label := fmt.Sprintf("state[%s]: stored usage response parses via shared contract", r.Name)
		reading, err := usage.Parse(snap.Vendor(), r.Body)
		chk(err == nil && reading.Drifted() == 0, label,
			"the response headroom itself stored no longer parses — shape drifted")
	}
}

// checkRouting audits the launch-routing state: the `.current` selection and
// the process environment. It runs from Run unconditionally — nothing here
// reads state.json, so no state-schema condition may suppress it.
func checkRouting(set accounts.Set, environ []string,
	chk, own func(bool, string, string)) {
	cfg, accts := set.Scope, set.Accounts

	// The launch path's own resolver, applied exactly as `headroom launch`
	// applies it: absent is the documented fresh-start default, while empty,
	// unreadable or naming a deleted account is corrupt routing state — launch
	// refuses on it, so the board is where the user hears about it first only
	// if this line is missing.
	bare, err := set.Bare()
	label := "current: .current resolves to a launchable account"
	switch {
	case err != nil:
	case bare.Auto:
		// Automatic placement is a valid routing state, not a missing one:
		// there is no single account, and a bare launch chooses.
		label = "current: .current says auto — a bare launch chooses the least-loaded account"
	default:
		label = fmt.Sprintf("%s (%s)", label, bare.Account.Name)
	}
	own(err == nil, label, fmt.Sprintf("%v — headroom launch refuses until it is fixed", err))

	// Not a failure and not drift: an inherited CLAUDE_CONFIG_DIR (a tmux
	// server started inside a Claude Code session, a nested shell) is
	// neutralized by every managed launch and every health probe headroom
	// spawns. It is reported because tools *outside* headroom that read the
	// variable are still being steered by it. Presence is the condition —
	// a present-but-empty value is unverified vendor territory, not the
	// verified absent state, so it is reported too. A *relative* value is the
	// stale-wrapper incident's signature and does fail: every unmanaged
	// `claude` in that shell runs as the primary while writing state beside
	// whatever directory it happens to start in (verified 2.1.220).
	homeVar := cfg.Env().HomeVar
	if v, present := launch.Ambient(cfg.Vendor, environ); present {
		if v != "" && !filepath.IsAbs(v) && cfg.Vendor == config.Claude {
			// Present-but-empty stays the ok-with-detail line below: it is
			// unverified vendor territory, not this signature.
			own(false, fmt.Sprintf("env: inherited CLAUDE_CONFIG_DIR is relative (%q)", v),
				"the stale-wrapper signature — unmanaged claude runs as the primary while writing state beside the cwd; exec zsh")
		} else {
			chk(true, fmt.Sprintf("env: inherited %s is neutralized by managed launches (%q)", homeVar, v), "")
		}
	}

	// The credential-redirect variable gets the same reporting: managed
	// launches strip it (obeying it would pair one account's tokens with
	// another's state — verified 2.1.220), unmanaged tools still obey it.
	for _, in := range launch.Redirects(cfg.Vendor, environ) {
		// A credential variable is named, never quoted.
		chk(true, fmt.Sprintf("env: inherited %s is neutralized by managed launches", in.String()), "")
	}

	// Every discovered config dir must be absolute: config.Load refuses
	// relative roots and launch.Extra refuses relative dirs, so a relative
	// path here means a construction this binary no longer performs — or a
	// bug in it.
	badDir := ""
	for _, a := range accts {
		if a.ConfigDir != "" && !filepath.IsAbs(a.ConfigDir) {
			badDir = a.ConfigDir
			break
		}
	}
	own(badDir == "", "dirs: every account config dir is absolute",
		fmt.Sprintf("%q is relative — claude would run it as the primary", badDir))

	// A relocated home (HEADROOM_HOME) re-points what headroom observes but
	// not what a primary launch would use — the primary is selected by the
	// variable being absent, resolved by the vendor against the real home.
	// Own-state, not drift: launch refuses the primary until it is unset.
	if cfg.PrimaryLaunchRefused() && cfg.Vendor == config.Claude {
		own(false, "home: HEADROOM_HOME re-points the primary headroom describes",
			"primary launches refuse; the board describes a tree bare `claude` would not use")
	}

	// Vendor lock debris in the accounts root: Claude Code's config locking
	// creates `<dir>.lock` directories, and a crash strands them where
	// discovery would otherwise adopt one as an account (observed 2026-08-10;
	// the health probe had seeded a skeleton .claude.json inside). Discovery
	// skips them; this line is where a stranded one is named instead of
	// silently vanishing from every surface.
	if entries, err := os.ReadDir(cfg.AccountsRoot); err == nil {
		for _, e := range entries {
			if e.IsDir() && accounts.LockArtifact(e.Name()) {
				chk(true, fmt.Sprintf("root: %s is vendor lock debris, not an account (skipped by discovery)", e.Name()),
					"")
			}
		}
	}

	// The shared-sessions topology is headroom's own invariant — the session
	// picker sees every conversation only while every extra's projects/ is a
	// symlink to the canonical store — and launch refuses on its violation,
	// so it is asserted here with the same shared verifier, per account:
	// accounts fail independently.
	for _, a := range accts {
		if a.IsPrimary() {
			continue
		}
		err := accounts.VerifyTopology(a)
		hint := ""
		if err != nil {
			hint = err.Error() + " — launches on this account refuse until it is fixed"
		}
		own(err == nil, fmt.Sprintf("topology[%s]: %s/ resolves to the canonical store", a.Name, cfg.StoreLink()), hint)
	}
}

// defaultCleanupPeriodDays is Claude Code's retention when no settings source
// sets cleanupPeriodDays (its documented retention_sweep event; read 2.1.287).
const defaultCleanupPeriodDays = 30

// checkRetention audits the one setting every account applies to the shared
// session store. Claude Code's cleanup sweep deletes transcripts older than
// the cleanupPeriodDays its config dir's settings.json names, and through the
// store link every account's sweep prunes the one shared store — so the store
// keeps only the shortest period any sharing account names, and one account
// left on the default quietly undoes a longer period set everywhere else.
// Only the user settings file varies by account; managed and project settings
// apply to every account alike. An unreadable settings.json is untested, not
// failed: Claude Code pauses its sweep when it cannot read its settings.
func checkRetention(set accounts.Set, own func(bool, string, string), skip func(string, string)) {
	var periods []string
	shortest, longest, unset := 0, 0, 0
	for _, a := range set.Accounts {
		if accounts.VerifyTopology(a) != nil {
			continue // not sharing the store; its topology line fails instead
		}
		name := a.Name
		if a.IsPrimary() {
			name = "primary"
		}
		days, isSet, err := cleanupPeriod(filepath.Join(a.Dir(), "settings.json"))
		if err != nil {
			skip(fmt.Sprintf("retention[%s]: not tested", name),
				fmt.Sprintf("%v — Claude Code pauses its cleanup sweep until it can read the setting", err))
			continue
		}
		label := fmt.Sprintf("%s %d", name, days)
		if !isSet {
			label += " (unset)"
			unset++
		}
		periods = append(periods, label)
		if shortest == 0 || days < shortest {
			shortest = days
		}
		longest = max(longest, days)
	}
	if len(periods) == 0 {
		return
	}
	label := fmt.Sprintf("retention: every account sharing the session store prunes it after %d days", shortest)
	if unset == len(periods) {
		label += " (cleanupPeriodDays unset — Claude Code's default)"
	}
	own(shortest == longest, label, fmt.Sprintf("accounts disagree on cleanupPeriodDays (%s) — every account's cleanup sweep prunes the shared store, so it keeps only %d days; "+
		"set one value in every account's settings.json (`accounts add --share-config` links them to one file)", strings.Join(periods, ", "), shortest))
}

// cleanupPeriod reads cleanupPeriodDays from one settings.json: absent file or
// key is the vendor default, unset.
func cleanupPeriod(path string) (days int, isSet bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaultCleanupPeriodDays, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var s struct {
		CleanupPeriodDays *int `json:"cleanupPeriodDays"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, false, fmt.Errorf("%s: %v", path, err)
	}
	switch {
	case s.CleanupPeriodDays == nil:
		return defaultCleanupPeriodDays, false, nil
	case *s.CleanupPeriodDays < 1:
		return 0, false, fmt.Errorf("%s: cleanupPeriodDays is %d, which Claude Code rejects (minimum 1)", path, *s.CleanupPeriodDays)
	}
	return *s.CleanupPeriodDays, true, nil
}

// checkHomes audits what a second home on this machine rests on: how this
// home's primary is selected, that no dir shares another's login, which ledger
// this home spends against, and that no account dir belongs to two homes.
// Every failure here is headroom's own state — a dir it seeded, a file it
// reads — never the vendor's.
func checkHomes(set accounts.Set, snap state.Snapshot, chk, own func(bool, string, string)) {
	scope := set.Scope
	if scope.PrimaryExplicit {
		chk(true, fmt.Sprintf("home: HOME is not this user's login home — the primary is launched by its dir (%s), never by absence", scope.PrimaryDir()), "")
	}

	// A login file linked between two dirs makes them one login, and a linked
	// history or registry puts one account's sessions under another's name.
	// Only dirs headroom launches by their dir are judged: the login home's
	// primary keeps ~/.claude.json outside its dir, where it is the vendor's.
	var linked []string
	for _, a := range set.Accounts {
		if a.ConfigDir == "" {
			continue
		}
		for _, path := range []string{a.MetaPath(), filepath.Join(a.Dir(), ".credentials.json"), filepath.Join(a.Dir(), "history.jsonl"), filepath.Join(a.Dir(), "sessions")} {
			if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				linked = append(linked, path)
			}
		}
	}
	own(len(linked) == 0, "logins: every account dir keeps its own login, history and session registry",
		fmt.Sprintf("%s is a link — a login shared between dirs is one login under two names; remove the link and log that dir in again", strings.Join(linked, ", ")))

	var shared []string
	mine := map[string]os.FileInfo{}
	for _, a := range set.Accounts {
		if fi, err := os.Stat(a.Dir()); err == nil {
			mine[a.Dir()] = fi
		}
	}
	var registered []config.Scope
	for _, m := range snap.Members() {
		registered = append(registered, config.HomeScope(scope.Vendor, m.Home, m.Root, m.Explicit))
	}
	homes := accounts.OtherHomes(scope, registered)
	others := len(homes)
	for _, h := range homes {
		for _, a := range h.Set.Accounts {
			fi, err := os.Stat(a.Dir())
			if err != nil {
				continue
			}
			for dir, ofi := range mine {
				if os.SameFile(fi, ofi) {
					shared = append(shared, fmt.Sprintf("%s (also %s's)", dir, h.Root))
				}
			}
		}
	}
	if snap.Shared() || others > 0 {
		whose := "this home's own"
		if snap.Shared() {
			whose = "another home's"
		}
		chk(true, fmt.Sprintf("ledger: %d other home(s) spend against this ledger (%s) — their sessions and launches count here", others, whose), "")
		own(len(shared) == 0, "homes: no account dir belongs to two homes",
			strings.Join(shared, ", ")+" — a launch in one home would run on the other's dir")
	}
}

func credentialSourceLabel(source creds.Source) string {
	switch source {
	case creds.SourceKeychain:
		return "keychain item under predicted name"
	case creds.SourceFile:
		return ".credentials.json (no keychain item)"
	default:
		return "no credential source"
	}
}

// rereadCredential samples the same source the board and initial check read.
func rereadCredential(primaryDir string) refresh.Reread {
	return func(keychainKey string) (string, bool) {
		dir := keychainKey
		if dir == "" {
			dir = primaryDir
		}
		raw, _ := creds.ReadRaw(keychainKey, dir)
		blob, ok := creds.Parse(raw)
		return blob.Token, ok
	}
}

func claudeBinary() string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// searchFile streams the (large) binary through searchReader.
func searchFile(path string, needles []string) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		return map[string]bool{}
	}
	defer f.Close()
	return searchReader(f, needles, 4<<20)
}

// searchReader scans r in bufSize chunks, keeping an overlap of the longest
// needle so a needle split across a chunk boundary is still found.
func searchReader(r io.Reader, needles []string, bufSize int) map[string]bool {
	found := map[string]bool{}
	overlap := 0
	for _, n := range needles {
		if len(n) > overlap {
			overlap = len(n)
		}
	}
	buf := make([]byte, bufSize)
	var tail []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(tail, buf[:n]...)
			for _, nd := range needles {
				if !found[nd] && bytes.Contains(chunk, []byte(nd)) {
					found[nd] = true
				}
			}
			if len(found) == len(needles) {
				return found
			}
			if len(chunk) > overlap {
				tail = append(tail[:0], chunk[len(chunk)-overlap:]...)
			} else {
				tail = chunk
			}
		}
		if err != nil {
			return found
		}
	}
}

// reportRequest is diagnostic policy over the same request evidence the board
// uses, for either vendor. Bookkeeping failures are reported first and
// unconditionally — they are independent of whatever the endpoint said — and
// the vendor only decides what a 401 means.
func reportRequest(vendor config.Vendor, name string, r refresh.Result, now time.Time, chk, own func(bool, string, string), skip func(string, string)) {
	status := "n/a"
	if r.Attempt.HTTPCode != 0 {
		status = fmt.Sprint(r.Attempt.HTTPCode)
	}
	label := fmt.Sprintf("api[%s]: HTTP %s", name, status)
	if r.StoreErr != nil {
		bookkeeping := fmt.Sprintf("state[%s]: request bookkeeping", name)
		if errors.Is(r.StoreErr, state.ErrBusy) || errors.Is(r.StoreErr, state.ErrReadOnly) {
			skip(bookkeeping, r.StoreErr.Error())
		} else {
			own(false, bookkeeping, r.StoreErr.Error())
		}
	}
	switch r.Attempt.State {
	case accountstate.AttemptStateUnavailable:
		skip(fmt.Sprintf("api[%s]: not tested", name), "headroom could not authorize a request against its state file")
	case accountstate.AttemptDeferred:
		skip(fmt.Sprintf("api[%s]: not tested", name), fmt.Sprintf("inside this account's quiet period — %s remaining", time.Unix(r.Attempt.NextEligibleAt, 0).Sub(now).Round(time.Second)))
	case accountstate.AttemptTransport:
		skip(label, "transport error — no evidence either way")
	case accountstate.AttemptRefused:
		skip(label, "rate limited — no evidence either way")
	case accountstate.AttemptUnparseable:
		chk(false, label+", unparseable body", "shape drifted")
	case accountstate.AttemptHTTP:
		switch {
		case r.Attempt.HTTPCode >= 500:
			skip(label, "vendor-side error — no evidence either way")
		case r.Attempt.HTTPCode == http.StatusUnauthorized && vendor == config.Codex && r.TokenAfter401 != refresh.TokenChanged:
			// Codex recovers from a 401 by reloading and refreshing, so a
			// rejected token is never evidence that anything drifted — where
			// Claude Code's unchanged-token 401 below is.
			skip(label, "access token rejected — any Codex session refreshes it; no evidence either way")
		case r.Attempt.HTTPCode == http.StatusUnauthorized && r.TokenAfter401 == refresh.TokenChanged:
			skip(label, "token was refreshed mid-check — no evidence either way")
		case r.Attempt.HTTPCode == http.StatusUnauthorized && r.TokenAfter401 == refresh.TokenUnknown:
			skip(label, "credential unreadable on re-check — cannot tell a refresh race from drift")
		default:
			chk(false, label, "unexpected status — endpoint or auth drifted")
		}
	case accountstate.AttemptOK, accountstate.AttemptNoLimits:
		bad := 0
		for _, row := range r.Observation.Rows {
			if row.Drifted() {
				bad++
			}
		}
		if r.Observation.Allowance.State == usage.AllowanceBad {
			bad++
		}
		hint := ""
		if bad > 0 {
			hint = fmt.Sprintf("%d malformed field(s) — shape drifted", bad)
		}
		chk(bad == 0, fmt.Sprintf("%s, %d row(s), fields well-formed", label, len(r.Observation.Rows)), hint)
	}
}
