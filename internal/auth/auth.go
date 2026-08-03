// Package auth asks Claude Code itself whether an account is logged in,
// rather than inferring it from credential timestamps.
//
// `claude auth status --json` is a supported, non-interactive, per-config-dir
// command that reads local state only: measured at ~170ms with sub-millisecond
// variance across runs, and every field it returns already lives in that
// account's .claude.json. So it costs no network, no usage-endpoint budget,
// and works offline.
//
// It answers exactly one question — is this account usable at all — and is
// deliberately not used for anything else. In particular it is never invoked
// to provoke a token refresh: refreshing belongs to Claude Code, and building
// on a side effect the vendor never promised would be the same mistake as
// reading expiresAt as account health.
package auth

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"

	"github.com/qiushiyan/headroom/internal/launch"
)

// Outcome separates the two ways there can be no answer, because they mean
// opposite things. A command that wouldn't run tells us nothing and headroom
// falls back to credential evidence. A command that ran and produced output
// we can't read is *drift* — the health oracle changed shape, `check` should
// fail on it, and the account's health is genuinely unknown rather than
// quietly inferred.
type Outcome uint8

const (
	// Unavailable is deliberately the zero value: an unset Status must mean
	// "no answer", never "logged out". A struct nobody filled in must not be
	// able to declare four healthy accounts dead.
	OutcomeUnavailable Outcome = iota // command absent, failed, or timed out
	OutcomeOK                         // answered
	OutcomeUnparseable                // ran, but the output no longer parses
	// Unrunnable: the probe's environment could not be safely constructed
	// (a config dir launch.Extra refuses, e.g. a relative path). Distinct
	// from Unavailable because the fallback differs: credential evidence is
	// read from the same broken dir spelling, so falling through to it would
	// convert a path-configuration bug into "not logged in — run /login".
	// Health is unknown here, and not inferable.
	OutcomeUnrunnable
)

// Status is the contract headroom needs from the auth command.
type Status struct {
	LoggedIn         bool
	Email            string
	SubscriptionType string
	Outcome          Outcome
}

// Answered reports whether this is a verdict rather than a lack of one.
func (s Status) Answered() bool { return s.Outcome == OutcomeOK }

// Parse applies the contract to the command's JSON. loggedIn is required —
// without it there is no answer, only noise.
func Parse(raw []byte) (Status, bool) {
	var doc struct {
		LoggedIn         *bool  `json:"loggedIn"`
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.LoggedIn == nil {
		return Status{Outcome: OutcomeUnparseable}, false
	}
	return Status{
		LoggedIn:         *doc.LoggedIn,
		Email:            doc.Email,
		SubscriptionType: doc.SubscriptionType,
		Outcome:          OutcomeOK,
	}, true
}

// QueryFunc is the seam the pipeline injects so prepare stays table-testable
// without a Claude Code install.
type QueryFunc func(configDir string) Status

// Query runs the command for one config dir ("" = the default ~/.claude).
// Every failure path returns an unparsed Status: the caller treats that as
// "health unknown" and falls back to credential evidence.
func Query(configDir string) Status {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "auth", "status", "--json")
	// The default dir is selected by the variable being absent, so an
	// inherited one (headroom run from inside a Claude Code session) must be
	// stripped or every account reports that session's login. launch owns
	// that rule; For is the sanctioned crossing for this dir-or-empty
	// parameter, and a dir it refuses must not be probed at all — the child
	// would answer with the default Keychain item's login, not this dir's.
	tgt, err := launch.For(configDir)
	if err != nil {
		return Status{Outcome: OutcomeUnrunnable}
	}
	cmd.Env = tgt.Env(os.Environ())
	return classify(cmd.Output())
}

// classify turns the command's result into an outcome. Output is judged
// before the exit status, because exec.Cmd.Output returns stdout alongside an
// ExitError: a command that printed a perfectly good answer and then exited
// non-zero has demonstrably run, and calling that "unavailable" would discard
// a real verdict — or, worse, hide genuine output drift behind an
// inconclusive.
func classify(out []byte, err error) Status {
	if len(out) > 0 {
		if st, ok := Parse(out); ok {
			return st
		}
		return Status{Outcome: OutcomeUnparseable}
	}
	if err != nil {
		return Status{Outcome: OutcomeUnavailable}
	}
	// Ran, succeeded, said nothing: the contract changed shape.
	return Status{Outcome: OutcomeUnparseable}
}
