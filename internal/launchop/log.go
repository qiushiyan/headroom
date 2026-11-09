package launchop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The launch log is one line per launch and per headless placement, beside
// state.json: where each went and why. It explains and no code reads it — a
// person reads it with jq — so it is appended and never rewritten, takes no
// lock, and a line that cannot be written is a note on the launch, never a
// refusal. Lines from older builds carry more fields and v 2; a reader keys on
// the fields below, which every version has.
type logLine struct {
	V       int    `json:"v"`
	At      string `json:"at"` // RFC3339 UTC
	Vendor  string `json:"vendor"`
	Mode    string `json:"mode"`   // auto, named or headless
	Reason  string `json:"reason"` // the rule's word for its choice
	Chosen  string `json:"chosen"` // the account, by its name
	CWD     string `json:"cwd"`
	Session string `json:"session"`       // the session id the launch named, "" for none
	PID     int    `json:"pid,omitempty"` // the launching process, which the exec keeps; 0 for a headless placement
	Problem string `json:"problem,omitempty"`
}

const logVersion = 3

// logPath is the log of one vendor's accounts root.
func logPath(accountsRoot string) string { return filepath.Join(accountsRoot, "launches.jsonl") }

// appendLog writes one line with O_APPEND, so lines from launches started
// together interleave whole.
func appendLog(accountsRoot string, l logLine, at time.Time) error {
	l.V, l.At = logVersion, at.UTC().Format(time.RFC3339)
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(logPath(accountsRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}
