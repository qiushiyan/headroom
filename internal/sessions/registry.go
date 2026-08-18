package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// LiveState is what the live-session registry plus a process probe establish
// about one transcript right now. Three values on purpose: "I could not
// verify" must not read as either safe or live — delete is refused on it,
// which errs toward keeping data.
type LiveState int

const (
	NotLive     LiveState = iota // no registry claim, or the pid provably isn't that session
	Live                         // registry claim confirmed: pid alive with matching start time
	LiveUnknown                  // registry claim present but unverifiable
)

// RegistryEntry is one account's claim that a session is currently open:
// a <pid>.json file under the account dir's sessions/. Written at session
// start and removed on clean exit, so presence is strong — but only pid
// liveness plus the recorded start instant makes it proof, because pids
// recycle. The instant compared is startedAt (epoch ms), never the
// procStart string: that one is rendered in UTC while ps renders local
// time, so string equality silently fails everywhere but UTC.
type RegistryEntry struct {
	Account     string // account name the claim belongs to
	SessionID   string
	PID         int
	StartedAtMS int64
	OK          bool // the file parsed and carries the fields above
}

// ReadRegistry parses every live-session record in one account dir.
func ReadRegistry(accountName, dir string) []RegistryEntry {
	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil {
		return nil
	}
	var out []RegistryEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		re := RegistryEntry{Account: accountName}
		data, err := os.ReadFile(filepath.Join(dir, "sessions", e.Name()))
		if err == nil {
			var rec struct {
				SessionID   string `json:"sessionId"`
				PID         int    `json:"pid"`
				StartedAtMS int64  `json:"startedAt"`
			}
			if json.Unmarshal(data, &rec) == nil && rec.SessionID != "" && rec.PID > 0 {
				re.SessionID, re.PID, re.StartedAtMS = rec.SessionID, rec.PID, rec.StartedAtMS
				re.OK = re.StartedAtMS > 0
			}
		}
		if re.SessionID == "" && !re.OK {
			// A malformed registry file names no session, so it can't guard
			// one; it is dropped rather than poisoning every row.
			continue
		}
		out = append(out, re)
	}
	return out
}

// ReadRegistryStrict is ReadRegistry for a caller about to destroy the
// account dir itself, where "could not read" must not read as "nothing
// live". Every registry file that fails to parse into a claim is returned
// as an unverifiable one (OK=false, SessionID naming the file), and a
// sessions/ dir that exists but cannot be listed is an error. The listing
// path keeps the tolerant reader: a malformed record there cannot guard a
// session it does not name, and dropping it keeps one bad file from
// poisoning every row.
func ReadRegistryStrict(accountName, dir string) ([]RegistryEntry, error) {
	sdir := filepath.Join(dir, "sessions")
	entries, err := os.ReadDir(sdir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RegistryEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		re := RegistryEntry{Account: accountName}
		data, err := os.ReadFile(filepath.Join(sdir, e.Name()))
		var rec struct {
			SessionID   string `json:"sessionId"`
			PID         int    `json:"pid"`
			StartedAtMS int64  `json:"startedAt"`
		}
		if err == nil && json.Unmarshal(data, &rec) == nil && rec.SessionID != "" && rec.PID > 0 {
			re.SessionID, re.PID, re.StartedAtMS = rec.SessionID, rec.PID, rec.StartedAtMS
			re.OK = re.StartedAtMS > 0
		} else {
			re.SessionID, re.OK = "registry file "+e.Name(), false
		}
		out = append(out, re)
	}
	return out, nil
}

// PIDProbe reports a process's start instant as epoch seconds. Injected by
// the caller — process inspection is an exec edge and stays out of this
// package.
type PIDProbe func(pid int) (startUnix int64, ok bool)

// startTolerance absorbs the skew between the vendor stamping startedAt and
// the kernel's process start: within it, the pid provably still belongs to
// that session. A recycled pid landing inside the window would need a new
// process born within seconds of the dead session's own birth.
const startTolerance = 10 // seconds

func startMatches(e RegistryEntry, startUnix int64) bool {
	d := startUnix - e.StartedAtMS/1000
	return d >= -startTolerance && d <= startTolerance
}

// liveRank orders states by strength of evidence: proof of life beats
// uncertainty beats absence. The enum's numeric order can't serve here —
// LiveUnknown's value is the largest, and comparing raw values once let an
// unverifiable stale claim demote a session the probe had *proven* open,
// past the resume guard that refuses only Live.
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

// Liveness resolves registry entries against a probe into per-session state.
// Verified live wins over unverifiable; a claim whose pid is gone or whose
// start instant mismatches (a recycled pid) is not live at all.
func Liveness(entries []RegistryEntry, probe PIDProbe) map[string]LiveState {
	m := map[string]LiveState{}
	set := func(id string, s LiveState) {
		if liveRank(s) > liveRank(m[id]) {
			m[id] = s
		}
	}
	for _, e := range entries {
		switch {
		case !e.OK || probe == nil:
			set(e.SessionID, LiveUnknown)
		default:
			start, ok := probe(e.PID)
			switch {
			case !ok:
				set(e.SessionID, NotLive) // pid gone: the claim is stale
			case startMatches(e, start):
				set(e.SessionID, Live)
			default:
				set(e.SessionID, NotLive) // pid recycled by another process
			}
		}
	}
	return m
}

// LiveNow re-establishes one session's liveness at action time. The
// listing's snapshot is for display; a mutation (or a resume refusal) must
// not trust it — a session opened after the picker drew would otherwise be
// renamed or deleted while a vendor process holds the file.
func LiveNow(id string, accts []AccountRef, probe PIDProbe) LiveState {
	var entries []RegistryEntry
	for _, a := range accts {
		entries = append(entries, ReadRegistry(a.Name, a.Dir)...)
	}
	return Liveness(entries, probe)[id]
}

// LiveAccount is which account holds a verified-live claim for a session —
// the strongest possible owner evidence: that account is driving it right
// now.
func LiveAccount(entries []RegistryEntry, probe PIDProbe, id string) (string, bool) {
	for _, e := range entries {
		if e.SessionID != id || !e.OK || probe == nil {
			continue
		}
		if start, ok := probe(e.PID); ok && startMatches(e, start) {
			return e.Account, true
		}
	}
	return "", false
}
