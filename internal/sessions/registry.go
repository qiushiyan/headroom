package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type LiveState int

const (
	NotLive LiveState = iota
	Live
	LiveUnknown
)

// RegistryEntry is a vendor claim; only a matching process start proves life.
type RegistryEntry struct {
	Account     string
	SessionID   string
	PID         int
	StartedAtMS int64
	OK          bool
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
			SessionID   string `json:"sessionId"`
			PID         int    `json:"pid"`
			StartedAtMS int64  `json:"startedAt"`
		}
		if err == nil {
			err = json.Unmarshal(data, &rec)
		}
		ok := err == nil && rec.SessionID != "" && rec.PID > 0 && rec.StartedAtMS > 0
		if !ok {
			out.Problems = append(out.Problems, fmt.Errorf("%s: unreadable session claim", path))
		}
		if rec.SessionID != "" {
			out.Entries = append(out.Entries, RegistryEntry{accountName, rec.SessionID, rec.PID, rec.StartedAtMS, ok})
		}
	}
	return out
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
	type sample struct {
		start int64
		err   error
	}
	samples := map[int]sample{}
	out := map[string]ProcessEvidence{}
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
			default:
				evidence.State = NotLive
			}
		}
		if liveRank(evidence.State) > liveRank(out[e.SessionID].State) {
			out[e.SessionID] = evidence
		}
	}
	return out
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
