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
	"strings"
	"time"
)

// Status is the contract headroom needs from the auth command. Parsed is
// false when the command produced nothing usable — which is "unknown", never
// "logged out": an absent binary must not read as a dead account.
type Status struct {
	LoggedIn         bool
	Email            string
	SubscriptionType string
	Parsed           bool
}

// Parse applies the contract to the command's JSON. loggedIn is required —
// without it there is no answer, only noise.
func Parse(raw []byte) (Status, bool) {
	var doc struct {
		LoggedIn         *bool  `json:"loggedIn"`
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.LoggedIn == nil {
		return Status{}, false
	}
	return Status{
		LoggedIn:         *doc.LoggedIn,
		Email:            doc.Email,
		SubscriptionType: doc.SubscriptionType,
		Parsed:           true,
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
	if configDir != "" {
		cmd.Env = append(environWithout("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR="+configDir)
	} else {
		// The default dir is selected by the variable being absent, so an
		// inherited one (headroom run from inside a Claude Code session)
		// must be stripped or every account reports that session's login.
		cmd.Env = environWithout("CLAUDE_CONFIG_DIR")
	}
	out, err := cmd.Output()
	if err != nil {
		return Status{}
	}
	st, _ := Parse(out)
	return st
}

// environWithout copies the environment minus one variable.
func environWithout(key string) []string {
	prefix := key + "="
	env := os.Environ()
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			kept = append(kept, kv)
		}
	}
	return kept
}
