package launchop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
)

// The launch log is one line per launch and per headless placement, beside
// state.json: where each went and why. It explains and no code reads it — a
// person reads it with jq — so it is appended and never rewritten, takes no
// lock, and a line that cannot be written is a note on the launch, never a
// refusal. Lines from older builds carry more fields and v 2, a v 3 line
// lacks what the choice rested on, and one before v 5 lacks the model fields;
// a reader keys on the fields down to Problem, which every version has.
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

	// What a choice the rule made rested on, as three numbers a person can add
	// up over weeks: how old the chosen account's figures were, how many
	// accounts shared its load (past one, the tie-break decided and not the
	// load), and how many the rule could not consider at all. They are derived
	// from the decision and are not its inputs: the candidates stay out of the
	// log. Absent on a named launch, where the rule chose nothing.
	AgeS     *int64 `json:"age_s,omitempty"`    // seconds since the chosen account was observed; absent when never
	Tied     int    `json:"tied,omitempty"`     // accounts with room at the chosen one's load, itself included
	Excluded int    `json:"excluded,omitempty"` // accounts the choice had to leave out

	// The model whose own limit admitted the launch, and the chosen account's
	// figure on that limit with what it rests on — named launches included,
	// since that limit admits them too. Absent when the launch asked for no
	// model with a limit of its own.
	Model      string `json:"model,omitempty"`       // the vendor's name for it ("Fable")
	ModelPct   *int   `json:"model_pct,omitempty"`   // the chosen account's counted figure
	ModelBasis string `json:"model_basis,omitempty"` // observed, stale (a lower bound), ended or bad
}

// rested fills the three numbers from the decision the store returned.
func (l *logLine) rested(d placement.Decision, now time.Time) {
	chosen, ok := d.Find(d.Chosen)
	if !ok {
		return
	}
	if chosen.ObservedAt > 0 {
		age := now.Unix() - chosen.ObservedAt
		l.AgeS = &age
	}
	for _, c := range d.Candidates {
		switch {
		case c.Excluded != "":
			l.Excluded++
		case !c.NearLimit && c.Load == chosen.Load:
			l.Tied++
		}
	}
}

// model fills the model fields from the decision.
func (l *logLine) model(d placement.Decision) {
	chosen, ok := d.Find(d.Chosen)
	if d.Model == "" || !ok || chosen.ModelRow == nil {
		return
	}
	pct := chosen.ModelRow.Counted
	l.Model, l.ModelPct, l.ModelBasis = d.Model, &pct, string(chosen.ModelRow.Basis)
}

const logVersion = 5

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
