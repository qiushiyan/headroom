// Package agy is what headroom knows of Google's Antigravity CLI: the one
// command it runs, the one line it reads from that run's log, and the
// envelope it stores the answer in. It reads nothing of agy's sign-in — not
// the Keychain item, not a token — because a program other than agy using
// that login is what Google suspends Antigravity accounts for; agy is the
// only client of its own login, and headroom reads what agy prints.
//
// Measured on agy 1.5.13 (docs/specs/agy-support.md § Antigravity observed);
// as perishable as every vendor contract, and `headroom check` re-proves it.
package agy

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Args is the quota read: the `/usage` slash command answered in print mode,
// as JSON, with the run's log written to logPath. Print mode answers a
// read-only slash command with no model turn — no tokens, no conversation,
// no history entry — and --log-file keeps the run out of agy's own log dir,
// so the one line identity is read from is this run's.
func Args(logPath string) []string {
	return []string{"-p", "/usage", "--output-format", "json", "--log-file", logPath}
}

// ExitAPIKey is the exit status of `/usage` under an API key, which has no
// account quota to show (documented in agy's changelog).
const ExitAPIKey = 2

// authLine opens the log line agy writes once a run has signed in:
// `applyAuthResult: email=<address>, authMethod=consumer, quotaProject=`.
var authLine = []byte("applyAuthResult: email=")

// EmailFromLog is the account a run signed in as: the email of the last
// applyAuthResult line in its log, "" when the log has none. A run that
// signed in again mid-way names its last sign-in, the one its answer came
// from.
func EmailFromLog(log []byte) string {
	email := ""
	for line := range bytes.Lines(log) {
		_, rest, ok := bytes.Cut(line, authLine)
		if !ok {
			continue
		}
		value, _, _ := bytes.Cut(rest, []byte(","))
		email = string(bytes.TrimSpace(value))
	}
	return email
}

// Envelope is the document headroom stores for one read: whose it is, from
// the run's own log, and agy's answer as it printed it. The answer
// names nobody, and nothing on disk names the signed-in Google account, so
// the identity travels inside the one stored body every reader replays.
type Envelope struct {
	Email  string          `json:"email"`
	Answer json.RawMessage `json:"agy"`
}

// ErrNotJSON is an answer that is not a JSON object: nothing to store.
var ErrNotJSON = errors.New("agy printed no JSON object")

// Wrap builds the stored document from one run's stdout and log. The answer
// keeps every value as agy printed it (the encoder drops only insignificant
// whitespace, as the store's does); one that is not a JSON object is refused
// here, so the store never holds a body no parser could read.
func Wrap(stdout, log []byte) ([]byte, error) {
	answer := bytes.TrimSpace(stdout)
	var probe map[string]json.RawMessage
	if json.Unmarshal(answer, &probe) != nil || probe == nil {
		// null decodes without an error, into no map at all.
		return nil, ErrNotJSON
	}
	return json.Marshal(Envelope{Email: EmailFromLog(log), Answer: answer})
}
