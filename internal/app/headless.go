package app

// `headroom headless`: the lane for a caller that runs Claude Code sessions
// headless under a config dir of its own — a skill-eval runner — and wants
// them spread over the subscriptions the way automatic launches spread.
// headroom decides and records; the caller starts the session itself, on the
// account's headless token, outside anything headroom builds or execs. So
// nothing here sets an environment or execs a session: the launch invariant
// (the child environment is a function of the decision) is untouched,
// because there is no child.
//
// A placement is the automatic launch's rule over the accounts that hold a
// headless token, recorded as load the way a launch is, with one difference:
// a caller that can wait is refused an account near a limit instead of being
// handed the least spent one, since an eval can wait and the person's own
// sessions on that account cannot. Spec: docs/specs/headless.md.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/usage"
)

// headlessSchema versions the documents `place` and the listing print.
const headlessSchema = 1

// Exit statuses of `headless place`: placed, or refused — the document says
// why, and when to ask again. A placement that could not be decided or
// recorded exits 1.
const (
	exitPlaced  = 0
	exitRefused = 3
)

func runHeadless(scope config.Scope, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "--json":
		if sub == "--json" {
			args = append([]string{"--json"}, args...)
		}
		return runHeadlessList(os.Stdout, scope, args, time.Now())
	case "mint":
		return runHeadlessMint(scope, args)
	case "place":
		return runHeadlessPlace(os.Stdout, os.Stderr, scope, args)
	case "started":
		return runHeadlessStarted(os.Stderr, scope, args)
	case "token":
		return runHeadlessToken(os.Stdout, os.Stderr, scope, args)
	case "forget":
		return runHeadlessForget(os.Stdout, os.Stderr, scope, args)
	default:
		fmt.Fprintf(os.Stderr, "headroom headless: unknown command %q — mint, place, started, token or forget\n", sub)
		return 2
	}
}

// ---- deciding ----

// gatherHeadless is gatherPlacement for a headless caller: the candidates a
// launch builds — figures, busy sessions, recent launches, from every home on
// the ledger — with each account's eligibility decided by its token instead
// of by whether its dir can be launched. A token is a grant of its own: the
// dir's login expiring, or its topology, says nothing about it.
//
// The caller's own exclusions join them as candidates' exclusions, so the
// rule decides among the rest in the same locked step that records the
// choice, and an account the caller will not take is never placed on: a
// session that ended on an account's usage limit or a token the vendor
// refused is the caller's to know, and that account reads idle to the rule.
func gatherHeadless(set accounts.Set, st *state.Store, tokens map[string]headless.Token, exclude map[string]bool, now time.Time) placeFacts {
	f := gatherPlacement(set, st, os.Environ(), true, now)
	for i := range f.cands {
		c := &f.cands[i]
		a, _ := f.account(c.Name)
		tok, ok := tokens[c.Name]
		c.Unlaunchable = false
		if exclude[c.Name] {
			c.Excluded = "excluded by the caller"
			continue
		}
		c.Excluded = headlessExclusion(a, f.keys[i], tok, ok, f.obs[i], now)
	}
	return f
}

// headlessExclusion is why a headless placement must not land on an account,
// or "". A token decides, with one more condition: the dir must still be
// logged in to the subscription the token was minted for, since that login
// is how headroom reads the figures — and so the load — the rule weighs.
func headlessExclusion(a accounts.Account, key state.Key, tok headless.Token, ok bool, obs *accountstate.Observation, now time.Time) string {
	switch {
	case !ok:
		return "no headless token"
	case tok.Ended(now):
		return "headless token ended " + when(tok.EndsMS, now)
	case key.ID() == tok.Key:
	case !a.Readable:
		return "its login could not be read just now"
	case key.UUID == "":
		return "not logged in, so its figures cannot be read — headroom login " + a.Name
	default:
		return "logged in to another subscription than its headless token — mint it again or forget it"
	}
	if obs != nil && obs.Allowance.State == usage.AllowanceBlocked {
		return "blocked by the vendor"
	}
	return ""
}

// headlessDoc is what `place` prints: the decision, and every account as the
// rule counted it. The token itself is never in it.
type headlessDoc struct {
	Schema    int    `json:"schema"`
	Placed    bool   `json:"placed"`
	DryRun    bool   `json:"dry_run,omitempty"`
	Account   string `json:"account,omitempty"`
	Placement string `json:"placement,omitempty"` // the id `started` reports the session's process against
	Reason    string `json:"reason,omitempty"`
	Rule      string `json:"rule"`

	// TokenEndsAt is when the chosen account's token ends; null when the
	// vendor did not say.
	TokenEndsAt *string `json:"token_ends_at,omitempty"`

	// Refusal says why nothing was placed, and RetryAt when the first account
	// set aside near a limit has room again — null when nothing says, and
	// then waiting alone will not help: an account needs a token, or a person.
	Refusal string  `json:"refusal,omitempty"`
	RetryAt *string `json:"retry_at"`

	Notes    []string       `json:"notes,omitempty"`
	Accounts []headlessAcct `json:"accounts"`
}

type headlessAcct struct {
	Account   string `json:"account"`
	Eligible  bool   `json:"eligible"`
	Excluded  string `json:"excluded,omitempty"`
	NearLimit bool   `json:"near_limit"`
	RoomAt    string `json:"room_at,omitempty"` // when an account near a limit has room again, when known

	Load    int `json:"load"`
	Busy    int `json:"busy"`
	Pending int `json:"pending"`

	ObservedAt *string         `json:"observed_at"` // null = never observed
	Stale      bool            `json:"stale"`       // older than fifteen minutes: the figures are lower bounds
	Session    *headlessFigure `json:"session"`
	Week       *headlessFigure `json:"week"`
}

// headlessFigure is one counted figure and what it rests on: "observed", a
// "stale" lower bound, a window that has "ended", or a percent that did not
// parse ("bad").
type headlessFigure struct {
	Percent  int     `json:"percent"`
	Basis    string  `json:"basis"`
	ResetsAt *string `json:"resets_at"`
}

func rfc(sec int64) *string {
	if sec <= 0 {
		return nil
	}
	s := time.Unix(sec, 0).UTC().Format(time.RFC3339)
	return &s
}

func headlessAccounts(d placement.Decision) []headlessAcct {
	out := make([]headlessAcct, 0, len(d.Candidates))
	for _, c := range d.Candidates {
		a := headlessAcct{
			Account: c.Name, Eligible: c.Excluded == "", Excluded: c.Excluded, NearLimit: c.NearLimit,
			Load: c.Load, Busy: c.Busy, Pending: c.Pending, ObservedAt: rfc(c.ObservedAt), Stale: c.Stale,
		}
		if c.NearLimit && c.Excluded == "" {
			if at := rfc(c.RoomAt()); at != nil {
				a.RoomAt = *at
			}
		}
		if s, ok := c.Session(); ok {
			a.Session = &headlessFigure{Percent: s.Counted, Basis: string(s.Basis), ResetsAt: rfc(s.ResetAt)}
		}
		if c.Week.Basis != "" {
			a.Week = &headlessFigure{Percent: c.Week.Counted, Basis: string(c.Week.Basis), ResetsAt: rfc(c.Week.ResetAt)}
		}
		out = append(out, a)
	}
	return out
}

// newPlacementID names one headless placement.
func newPlacementID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runHeadlessPlace decides which account the caller's next session spends,
// records the decision as load and prints it. It refuses — exit 3, nothing
// recorded — when every account it could take is near a limit, excluded, or
// blocked, and says when room returns.
func runHeadlessPlace(out, errw io.Writer, scope config.Scope, args []string) int {
	prefer, dryRun := "", false
	var excluded []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--dry-run":
			dryRun = true
		case "--prefer", "--exclude":
			if i+1 >= len(args) || args[i+1] == "" {
				fmt.Fprintf(errw, "headroom headless place: %s needs an account name\n", a)
				return 2
			}
			i++
			if a == "--prefer" {
				prefer = args[i]
			} else {
				excluded = append(excluded, args[i])
			}
		default:
			fmt.Fprintf(errw, "headroom headless place: unknown argument %q\n", a)
			return 2
		}
	}
	tokens, err := headless.Read(scope.AccountsRoot)
	if err != nil {
		fmt.Fprintf(errw, "headroom headless place: %v\n", err)
		return 1
	}
	set := accounts.Discover(scope)
	intent := placement.Intent{Kind: placement.Auto, CanWait: true}
	if prefer != "" {
		// The account a session ran on last: followed while it has room, so
		// the session's prompt cache stays warm, as a launch follows the owner
		// of a session it names.
		a, err := set.Select(prefer)
		if err != nil {
			fmt.Fprintf(errw, "headroom headless place: %v\n", err)
			return 1
		}
		intent.Owner = a.Name
	}
	// Names the caller was handed by an earlier placement: one nothing
	// answers to is a caller's mistake, said rather than silently ignored.
	exclude := map[string]bool{}
	for _, name := range excluded {
		a, err := set.Select(name)
		if err != nil {
			fmt.Fprintf(errw, "headroom headless place: --exclude: %v\n", err)
			return 1
		}
		exclude[a.Name] = true
	}
	if exclude[intent.Owner] {
		fmt.Fprintf(errw, "headroom headless place: %s is both preferred and excluded\n", intent.Owner)
		return 2
	}
	now := time.Now()
	st := state.Open(scope)
	intent.Home = st.Home()
	facts := gatherHeadless(set, st, tokens, exclude, now)

	doc := headlessDoc{Schema: headlessSchema, DryRun: dryRun}
	var d placement.Decision
	failed := false
	if dryRun {
		d = placement.Choose(facts.cands, facts.snap.Placements(), intent, now)
	} else {
		id := newPlacementID()
		placed, err := st.Place(state.Launch{Candidates: facts.cands, Intent: intent, Now: now, ID: id, Gone: facts.gone})
		d = placed.Decision
		switch {
		case err != nil:
			// A placement nobody can see is a session the person's launches
			// cannot steer around, so an unrecorded one is not handed out.
			d.Chosen, d.Refusal, d.RoomAt = "", fmt.Sprintf("the placement could not be recorded (%v)", err), 0
			failed = true
			fmt.Fprintf(errw, "headroom headless place: %s\n", d.Refusal)
		case d.Chosen != "":
			doc.Placement = id
			logHeadless(scope, facts, placed, intent, now, &doc)
			if refreshWorthStarting(facts, now) {
				_ = startRefresh(scope)
			}
		}
	}
	doc.Rule, doc.Accounts = d.Rule, headlessAccounts(d)
	code := exitPlaced
	if d.Chosen == "" {
		doc.Refusal, doc.RetryAt, code = d.Refusal, rfc(d.RoomAt), exitRefused
		if failed {
			code = 1
		}
	} else {
		doc.Placed, doc.Account, doc.Reason = true, d.Chosen, d.Reason
		if tok := tokens[d.Chosen]; tok.EndsMS > 0 {
			doc.TokenEndsAt = rfc(tok.EndsMS / 1000)
			if left := time.UnixMilli(tok.EndsMS).Sub(now); left < defaultLoginWithin {
				doc.Notes = append(doc.Notes, fmt.Sprintf("its headless token ends %s — headroom headless mint %s", when(tok.EndsMS, now), d.Chosen))
			}
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return 1
	}
	return code
}

// logHeadless writes the placement's line in the launch log, as a launch's:
// `launches --eval` judges the rule by every decision it made.
func logHeadless(scope config.Scope, facts placeFacts, placed state.Placed, intent placement.Intent, now time.Time, doc *headlessDoc) {
	rec := launchlog.New(placed.Decision, facts.cands, placed.Ledger, intent, now)
	rec.Vendor, rec.Mode, rec.Recorded = string(scope.Vendor), "headless", placed.Recorded
	rec.CWD, _ = os.Getwd()
	if err := launchlog.Append(scope.AccountsRoot, rec); err != nil {
		doc.Notes = append(doc.Notes, fmt.Sprintf("launch log not written (%v)", err))
	}
}

// ---- the session it became ----

// runHeadlessStarted records the process a caller started on a placement, so
// the session counts as busy on that account for as long as it runs — not
// only for the fifteen minutes a placement counts — and is counted once. A
// process already gone has nothing more to count; its placement still counts
// for its span.
func runHeadlessStarted(errw io.Writer, scope config.Scope, args []string) int {
	if len(args) != 2 || args[0] == "" {
		fmt.Fprintln(errw, "usage: headroom headless started <placement> <pid>")
		return 2
	}
	id := args[0]
	pid, err := strconv.Atoi(args[1])
	if err != nil || pid <= 0 {
		fmt.Fprintf(errw, "headroom headless started: %q is not a process id\n", args[1])
		return 2
	}
	start, err := placementProbe(pid)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(errw, "headroom headless started: process %d has already exited — its placement counts as a launch\n", pid)
		return 0
	}
	if err != nil {
		fmt.Fprintf(errw, "headroom headless started: cannot read when process %d started: %v\n", pid, err)
		return 1
	}
	st := state.Open(scope)
	gone := goneRuns(st.Load().Runs())
	if _, err := st.Started(id, pid, start*1000, gone, time.Now()); err != nil {
		if errors.Is(err, state.ErrNotPlaced) {
			err = fmt.Errorf("%w (%s): a placement counts for %s and is then forgotten", err, id, placement.PendingFor)
		}
		fmt.Fprintf(errw, "headroom headless started: %v\n", err)
		return 1
	}
	return 0
}

// goneRuns is the runs whose process is proven over, from one sample per pid.
func goneRuns(runs []state.Run) []state.Run {
	entries := make([]sessions.RegistryEntry, len(runs))
	for i, r := range runs {
		entries[i] = runEntry("", r)
	}
	evidence, _ := sessions.InspectClaims(entries, sampleOnce(placementProbe))
	var gone []state.Run
	for i, r := range runs {
		if evidence[entries[i].SessionID].State == sessions.NotLive {
			gone = append(gone, r)
		}
	}
	return gone
}

// ---- the credential ----

// runHeadlessToken prints one account's headless token and nothing else: the
// only path by which a token leaves its file. It refuses a token that has
// ended, and one minted for another login than the dir now holds.
func runHeadlessToken(out, errw io.Writer, scope config.Scope, args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(errw, "usage: headroom headless token <account>")
		return 2
	}
	a, err := accounts.Discover(scope).Select(args[0])
	if err != nil {
		fmt.Fprintf(errw, "headroom headless token: %v\n", err)
		return 1
	}
	tokens, err := headless.Read(scope.AccountsRoot)
	if err != nil {
		fmt.Fprintf(errw, "headroom headless token: %v\n", err)
		return 1
	}
	tok, ok := tokens[a.Name]
	now := time.Now()
	key := state.KeyFor(a)
	switch {
	case !ok:
		fmt.Fprintf(errw, "headroom headless token: %s has no headless token — headroom headless mint %s\n", a.Name, a.Name)
		return 1
	case tok.Ended(now):
		fmt.Fprintf(errw, "headroom headless token: %s's headless token ended %s — headroom headless mint %s\n", a.Name, when(tok.EndsMS, now), a.Name)
		return 1
	case key.UUID != "" && key.ID() != tok.Key:
		// Positive evidence only: a dir whose identity cannot be read right
		// now still holds the subscription the token was minted for.
		fmt.Fprintf(errw, "headroom headless token: %s is now logged in to another subscription than its token — mint it again or forget it\n", a.Name)
		return 1
	}
	fmt.Fprintln(out, tok.Value)
	return 0
}

func runHeadlessForget(out, errw io.Writer, scope config.Scope, args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(errw, "usage: headroom headless forget <account>")
		return 2
	}
	// A name with a stored token is forgotten even if its dir has gone.
	name := args[0]
	if a, err := accounts.Discover(scope).Select(name); err == nil {
		name = a.Name
	}
	removed, err := headless.Remove(scope.AccountsRoot, name)
	if err != nil {
		fmt.Fprintf(errw, "headroom headless forget: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(errw, "headroom headless forget: %s has no headless token\n", name)
		return 1
	}
	fmt.Fprintf(out, "forgot %s's headless token — headroom does not revoke it with the vendor\n", name)
	return 0
}

// ---- the listing ----

type headlessListDoc struct {
	Schema   int                `json:"schema"`
	Accounts []headlessListAcct `json:"accounts"`
}

type headlessListAcct struct {
	Account  string  `json:"account"`
	Token    bool    `json:"token"`
	MintedAt *string `json:"minted_at,omitempty"`
	EndsAt   *string `json:"ends_at,omitempty"`
	Excluded string  `json:"excluded,omitempty"` // why a placement would not choose it
}

// runHeadlessList shows which accounts a headless placement chooses from and
// when each token ends. Values are never shown.
func runHeadlessList(out io.Writer, scope config.Scope, args []string, now time.Time) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			fmt.Fprintf(os.Stderr, "headroom headless: unexpected argument %q\n", a)
			return 2
		}
		asJSON = true
	}
	tokens, err := headless.Read(scope.AccountsRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom headless: %v\n", err)
		return 1
	}
	st := state.Open(scope)
	facts, _ := accountstate.Assemble(accounts.Discover(scope), st.Load(), now)
	doc := headlessListDoc{Schema: headlessSchema}
	for _, fa := range facts {
		tok, ok := tokens[fa.Acct.Name]
		row := headlessListAcct{Account: fa.Acct.Name, Token: ok, Excluded: headlessExclusion(fa.Acct, fa.Key, tok, ok, fa.View.Obs, now)}
		if ok {
			row.MintedAt = rfc(tok.MintedMS / 1000)
			row.EndsAt = rfc(tok.EndsMS / 1000)
		}
		doc.Accounts = append(doc.Accounts, row)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return 1
		}
		return 0
	}
	nameW := len("account")
	for _, r := range doc.Accounts {
		nameW = max(nameW, render.Cells(render.Sanitize(r.Account)))
	}
	fmt.Fprintf(out, "  %s  headless token\n", render.PadCell("account", nameW))
	for _, r := range doc.Accounts {
		tok := tokens[r.Account]
		var says []string
		switch {
		case !r.Token:
			says = append(says, "none — headroom headless mint "+r.Account)
		case tok.EndsMS > 0 && !tok.Ended(now):
			says = append(says, "ends "+when(tok.EndsMS, now))
		case tok.EndsMS == 0:
			says = append(says, "end not stated by the vendor")
		}
		if r.Token && r.Excluded != "" {
			says = append(says, r.Excluded)
		}
		fmt.Fprintf(out, "  %s  %s\n", render.PadCell(render.Sanitize(r.Account), nameW), strings.Join(says, " · "))
	}
	return 0
}
