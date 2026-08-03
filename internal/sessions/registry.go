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

// Liveness resolves registry entries against a probe into per-session state.
// Verified live wins over unverifiable; a claim whose pid is gone or whose
// start instant mismatches (a recycled pid) is not live at all.
func Liveness(entries []RegistryEntry, probe PIDProbe) map[string]LiveState {
	m := map[string]LiveState{}
	set := func(id string, s LiveState) {
		if s > m[id] {
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
