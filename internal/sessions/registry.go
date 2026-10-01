package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/tag"
)

type LiveState int

const (
	NotLive LiveState = iota
	Live
	LiveUnknown
)

// RegistryEntry is a vendor claim; only a matching process start proves life.
//
// Status is the vendor's own word for what the session is doing, carried
// verbatim ("busy" and "idle" observed on 2.1.286, "shell" seen once): only
// StatusBusy is given a meaning here, and every other value is passed through
// as it was read. StatusState says whether there was a word at all — absent,
// or present under a type it has never had, is not "idle".
type RegistryEntry struct {
	Account     string
	SessionID   string
	PID         int
	StartedAtMS int64
	OK          bool
	Status      string
	StatusState tag.State
}

// StatusBusy is the one registry status headroom acts on: the session is
// working a turn right now.
const StatusBusy = "busy"

// Busy reports a session the vendor says is working. Any other status,
// including none, is not busy and is not thereby idle.
func (e RegistryEntry) Busy() bool {
	return e.StatusState == tag.OK && e.Status == StatusBusy
}

// Registry preserves every identifiable claim and reports unreadable evidence.
// A nameless damaged file cannot identify a transcript, but it prevents removal
// of its account. An unreadable directory prevents session mutations as well.
type Registry struct {
	Entries    []RegistryEntry
	Problems   []error
	Unreadable bool
}

func ReadRegistry(accountName, dir string) Registry {
	var out Registry
	sdir := filepath.Join(dir, "sessions")
	entries, err := os.ReadDir(sdir)
	if err != nil {
		if !os.IsNotExist(err) {
			out.Problems = append(out.Problems, err)
			out.Unreadable = true
		}
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(sdir, e.Name())
		data, err := os.ReadFile(path)
		var rec struct {
			SessionID   string          `json:"sessionId"`
			PID         int             `json:"pid"`
			StartedAtMS int64           `json:"startedAt"`
			Status      json.RawMessage `json:"status"`
		}
		if err == nil {
			err = json.Unmarshal(data, &rec)
		}
		ok := err == nil && rec.SessionID != "" && rec.PID > 0 && rec.StartedAtMS > 0
		if !ok {
			out.Problems = append(out.Problems, fmt.Errorf("%s: unreadable session claim", path))
		}
		if rec.SessionID != "" {
			status, statusState := decodeStatus(rec.Status)
			out.Entries = append(out.Entries, RegistryEntry{Account: accountName, SessionID: rec.SessionID,
				PID: rec.PID, StartedAtMS: rec.StartedAtMS, OK: ok, Status: status, StatusState: statusState})
		}
	}
	return out
}

// decodeStatus reads the status field as a non-empty string. Absent or null
// is tag.None; anything else that is not a non-empty string is tag.Bad.
func decodeStatus(raw json.RawMessage) (string, tag.State) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", tag.None
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", tag.Bad
	}
	return s, tag.OK
}

// PIDProbe returns a start instant, os.ErrNotExist for a proven absent process,
// or an inspection error. Failure to inspect is never evidence of absence.
type PIDProbe func(pid int) (startUnix int64, err error)

const startTolerance = 10 // seconds between kernel start and vendor registration
func startMatches(e RegistryEntry, startUnix int64) bool {
	d := startUnix - e.StartedAtMS/1000
	return d >= -startTolerance && d <= startTolerance
}

type ProcessEvidence struct {
	State   LiveState
	Account string // populated only by a verified-live claim
}

func liveRank(s LiveState) int {
	switch s {
	case Live:
		return 2
	case LiveUnknown:
		return 1
	default:
		return 0
	}
}

// Inspect samples each PID once. Ownership and liveness consume the same facts.
func Inspect(entries []RegistryEntry, probe PIDProbe) map[string]ProcessEvidence {
	evidence, _ := InspectClaims(entries, probe)
	return evidence
}

// InspectClaims is Inspect plus the claims it verified as live — the pid runs
// and its kernel start matches the registry's — from the same one sample per
// pid, so a session's liveness and an account's live sessions cannot be
// decided two ways. A claim whose pid was recycled, or whose record is
// damaged, is not among them.
func InspectClaims(entries []RegistryEntry, probe PIDProbe) (map[string]ProcessEvidence, []RegistryEntry) {
	type sample struct {
		start int64
		err   error
	}
	samples := map[int]sample{}
	out := map[string]ProcessEvidence{}
	var live []RegistryEntry
	for _, e := range entries {
		evidence := ProcessEvidence{State: LiveUnknown}
		if e.OK && probe != nil {
			p, ok := samples[e.PID]
			if !ok {
				p.start, p.err = probe(e.PID)
				samples[e.PID] = p
			}
			switch {
			case errors.Is(p.err, os.ErrNotExist):
				evidence.State = NotLive
			case p.err != nil:
			case startMatches(e, p.start):
				evidence = ProcessEvidence{Live, e.Account}
				live = append(live, e)
			default:
				evidence.State = NotLive
			}
		}
		if liveRank(evidence.State) > liveRank(out[e.SessionID].State) {
			out[e.SessionID] = evidence
		}
	}
	return out, live
}

// LiveNow re-reads claims immediately before an action; collection can age.
func LiveNow(id string, accts []AccountRef, probe PIDProbe) LiveState {
	var entries []RegistryEntry
	unreadable := false
	for _, a := range accts {
		reg := ReadRegistry(a.Name, a.Dir)
		entries = append(entries, reg.Entries...)
		unreadable = unreadable || reg.Unreadable
	}
	state := Inspect(entries, probe)[id].State
	if unreadable && state != Live {
		return LiveUnknown
	}
	return state
}
