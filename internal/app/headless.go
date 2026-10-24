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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

// headlessSchema versions the documents `place` and the listing print.
const headlessSchema = 1

// ExitRefused is `headless place`'s status for a refusal: every account it
// could take is near a limit or excluded, and the document printed says why
// and when to ask again. A placement that could not be decided or recorded
// fails as any command does.
const ExitRefused = 3

// ---- deciding ----

// tokenProblem is why a's headless token must not be used, or "": there is
// none, it has ended, or it was minted for another subscription than the one
// a's dir (whose key is key) is logged in to now. Positive evidence only: a
// dir that is logged out, or whose identity cannot be read just now, still
// holds the subscription its token was minted for. `headless token` refuses
// on it alone; a placement asks more (headlessExclusion).
func tokenProblem(a accounts.Account, key state.Key, tok headless.Token, ok bool, now time.Time) string {
	switch {
	case !ok:
		return "no headless token — headroom headless mint " + a.Name
	case tok.Ended(now):
		return "headless token ended " + render.When(tok.EndsMS, now) + " — headroom headless mint " + a.Name
	case key.UUID != "" && key.ID() != tok.Key:
		return "logged in to another subscription than its headless token — mint it again or forget it"
	}
	return ""
}

// headlessExclusion is why a headless placement must not land on an account,
// or "". The token decides, with one more condition: the dir must be logged
// in, to the subscription the token was minted for, since that login is how
// headroom reads the figures — and so the load — the rule weighs.
func headlessExclusion(a accounts.Account, key state.Key, tok headless.Token, ok bool, now time.Time) string {
	switch why := tokenProblem(a, key, tok, ok, now); {
	case why != "":
		return why
	case key.UUID != "":
		return ""
	case !a.Readable:
		return "its login could not be read just now"
	}
	return "not logged in, so its figures cannot be read — headroom login " + a.Name
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

// HeadlessPlaceOptions are a headless caller's ask: the account its session
// ran on last, the accounts it will not take, and whether to record nothing.
type HeadlessPlaceOptions struct {
	Prefer  string
	Exclude []string
	DryRun  bool
}

// HeadlessPlace decides which account the caller's next session spends,
// records the decision as load and prints it. It refuses — exit 3, nothing
// recorded — when every account it could take is near a limit or excluded,
// and says when room returns.
func HeadlessPlace(std Streams, scope config.Scope, opts HeadlessPlaceOptions, place launchop.Deps) error {
	tokens, err := headless.Read(scope.AccountsRoot)
	if err != nil {
		return err
	}
	set := accounts.Discover(scope)
	preferred := ""
	if opts.Prefer != "" {
		// The account a session ran on last: followed while it has room, so
		// the session's prompt cache stays warm, as a launch follows the owner
		// of a session it names.
		a, err := set.Select(opts.Prefer)
		if err != nil {
			return err
		}
		preferred = a.Name
	}
	// Names the caller was handed by an earlier placement: one nothing
	// answers to is a caller's mistake, said rather than silently ignored.
	exclude := map[string]bool{}
	for _, name := range opts.Exclude {
		a, err := set.Select(name)
		if err != nil {
			return fmt.Errorf("--exclude: %w", err)
		}
		exclude[a.Name] = true
	}
	if exclude[preferred] {
		return usagef("%s is both preferred and excluded", preferred)
	}
	now := time.Now()
	cwd, _ := os.Getwd()
	placed := launchop.PlaceHeadless(launchop.HeadlessRequest{
		Set: set, Store: state.Open(scope), Prefer: preferred, Exclude: exclude, DryRun: opts.DryRun, CWD: cwd, Now: now,
		Eligible: func(a accounts.Account, key state.Key) string {
			tok, ok := tokens[a.Name]
			return headlessExclusion(a, key, tok, ok, now)
		},
	}, place)

	doc := headlessDoc{Schema: headlessSchema, DryRun: opts.DryRun, Placement: placed.ID, Notes: placed.Notes}
	d := placed.Decision
	doc.Rule, doc.Accounts = d.Rule, headlessAccounts(d)
	var outcome error
	if d.Chosen == "" {
		doc.Refusal, doc.RetryAt, outcome = d.Refusal, rfc(d.RoomAt), Status(ExitRefused)
		if placed.Unrecorded != nil {
			outcome = errors.New(d.Refusal)
		}
	} else {
		doc.Placed, doc.Account, doc.Reason = true, d.Chosen, d.Reason
		if tok := tokens[d.Chosen]; tok.EndsMS > 0 {
			doc.TokenEndsAt = rfc(tok.EndsMS / 1000)
			if left := time.UnixMilli(tok.EndsMS).Sub(now); left < defaultLoginWithin {
				doc.Notes = append(doc.Notes, fmt.Sprintf("its headless token ends %s — headroom headless mint %s", render.When(tok.EndsMS, now), d.Chosen))
			}
		}
	}
	enc := json.NewEncoder(std.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return outcome
}

// ---- the session it became ----

// HeadlessStarted records the process a caller started on a placement, so
// the session counts as busy on that account for as long as it runs — not
// only for the fifteen minutes a placement counts — and is counted once. A
// process already gone has nothing more to count; its placement still counts
// for its span.
func HeadlessStarted(std Streams, scope config.Scope, id string, pid int, place launchop.Deps) error {
	err := launchop.Started(state.Open(scope), id, pid, time.Now(), place)
	if errors.Is(err, launchop.ErrExited) {
		fmt.Fprintf(std.Err, "headroom headless started: process %d has already exited — its placement counts as a launch\n", pid)
		return nil
	}
	return err
}

// ---- the credential ----

// HeadlessToken prints one account's headless token and nothing else: the
// only path by which a token leaves its file. It refuses a token that has
// ended, and one minted for another login than the dir now holds.
func HeadlessToken(std Streams, scope config.Scope, name string) error {
	a, err := accounts.Discover(scope).Select(name)
	if err != nil {
		return err
	}
	tokens, err := headless.Read(scope.AccountsRoot)
	if err != nil {
		return err
	}
	tok, ok := tokens[a.Name]
	if why := tokenProblem(a, state.KeyFor(a), tok, ok, time.Now()); why != "" {
		return fmt.Errorf("%s: %s", a.Name, why)
	}
	fmt.Fprintln(std.Out, tok.Value)
	return nil
}

// HeadlessForget forgets one account's headless token. headroom does not
// revoke it with the vendor.
func HeadlessForget(std Streams, scope config.Scope, name string) error {
	// A name with a stored token is forgotten even if its dir has gone.
	if a, err := accounts.Discover(scope).Select(name); err == nil {
		name = a.Name
	}
	removed, err := headless.Remove(scope.AccountsRoot, name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("%s has no headless token", name)
	}
	fmt.Fprintf(std.Out, "forgot %s's headless token — headroom does not revoke it with the vendor\n", name)
	return nil
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

// HeadlessList shows which accounts a headless placement chooses from and
// when each token ends. Values are never shown.
func HeadlessList(std Streams, scope config.Scope, asJSON bool, now time.Time) error {
	out := std.Out
	tokens, err := headless.Read(scope.AccountsRoot)
	if err != nil {
		return err
	}
	st := state.Open(scope)
	facts, _ := accountstate.Assemble(accounts.Discover(scope), st.Load(), now)
	doc := headlessListDoc{Schema: headlessSchema}
	for _, fa := range facts {
		tok, ok := tokens[fa.Acct.Name]
		row := headlessListAcct{Account: fa.Acct.Name, Token: ok, Excluded: headlessExclusion(fa.Acct, fa.Key, tok, ok, now)}
		if ok {
			row.MintedAt = rfc(tok.MintedMS / 1000)
			row.EndsAt = rfc(tok.EndsMS / 1000)
		}
		doc.Accounts = append(doc.Accounts, row)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
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
			says = append(says, "ends "+render.When(tok.EndsMS, now))
		case tok.EndsMS == 0:
			says = append(says, "end not stated by the vendor")
		}
		if r.Token && r.Excluded != "" {
			says = append(says, r.Excluded)
		}
		fmt.Fprintf(out, "  %s  %s\n", render.PadCell(render.Sanitize(r.Account), nameW), strings.Join(says, " · "))
	}
	return nil
}
