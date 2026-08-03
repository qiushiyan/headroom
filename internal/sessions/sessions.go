package sessions

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// OwnerState says how a session's owner account was established — carried to
// the surface and `--json`, because "why that account?" must be answerable.
type OwnerState int

const (
	OwnerNone     OwnerState = iota // no evidence at all → caller falls back to current
	OwnerLive                       // verified live registry: that account runs it now
	OwnerRehome                     // the user's explicit re-home record
	OwnerHistory                    // newest prompt in that account's history
	OwnerMissing                    // best evidence names an account that no longer exists
	OwnerConflict                   // two accounts claim the same newest moment
)

// Session is one listable transcript plus everything the picker needs to
// render and act on it. Axes stay separate on purpose: what the transcript
// says (Tail), whether its project dir still exists (DirOK), whether it is
// open right now (Live), and which account should resume it (Owner) vary
// independently, and collapsing them is the class of bug this repo's model
// exists to prevent.
type Session struct {
	ID       string
	Path     string // the transcript
	StoreDir string // its containing store directory (full path)
	MTime    time.Time
	Size     int64

	Tail Tail

	CWD      string // munge-verified resume target; "" = not recoverable
	DirOK    bool   // CWD is non-empty and exists right now
	RepoKey  string // canonical main-checkout path; "" = none
	RepoRoot string // the checkout/worktree root containing CWD
	Local    bool   // same repo (or same dir) as the picker's cwd

	Owner      string // account name; "" when OwnerState says there is nothing to name
	OwnerState OwnerState
	Live       LiveState
}

// AccountRef names one discovered account and its real config dir.
type AccountRef struct {
	Name string
	Dir  string
}

// Input wires Collect. Exec stays outside: Probe is the one process
// inspection the liveness check needs, injected by the app layer.
type Input struct {
	ProjectsDir string
	CWD         string // where the picker runs; anchors the local section
	Accounts    []AccountRef
	OwnersPath  string
	Probe       PIDProbe
}

// Listing is the collector's whole answer, shared by the picker and --json.
type Listing struct {
	Sessions []*Session // newest first
	LocalKey string     // repo key (or canonical dir) the local section groups on
}

// Collect walks the store once, synchronously — measured at ~50ms for the
// live store's 178 transcripts via TailBudget-bounded reads, which is under
// any threshold worth a cache or a skeleton frame, and a cache over vendor
// files would be a second source of truth with its own invalidation bugs.
func Collect(in Input) Listing {
	owners := LoadOwners(in.OwnersPath)
	names := map[string]bool{}
	hists := map[string]History{}
	var registry []RegistryEntry
	for _, a := range in.Accounts {
		names[a.Name] = true
		if f, err := os.Open(filepath.Join(a.Dir, "history.jsonl")); err == nil {
			hists[a.Name] = ParseHistory(f)
			f.Close()
		}
		registry = append(registry, ReadRegistry(a.Name, a.Dir)...)
	}
	liveness := Liveness(registry, in.Probe)

	repos := repoCache{}
	byID := map[string]*Session{}
	dirs, _ := os.ReadDir(in.ProjectsDir)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		storeDir := filepath.Join(in.ProjectsDir, d.Name())
		files, _ := os.ReadDir(storeDir)
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			s := readSession(storeDir, d.Name(), f.Name())
			if s == nil {
				continue
			}
			// Orphan-suffixed duplicates share the id of their sibling; one
			// row per session, preferring the canonical file, else the newer.
			if prev, ok := byID[s.ID]; ok {
				if prevCanonical := prev.Path == filepath.Join(prev.StoreDir, prev.ID+".jsonl"); prevCanonical {
					continue
				}
				if s.Path != filepath.Join(storeDir, s.ID+".jsonl") && !s.MTime.After(prev.MTime) {
					continue
				}
			}
			byID[s.ID] = s
		}
	}

	local := repos.lookup(in.CWD)
	localKey := local.Key
	if localKey == "" {
		localKey = canon(in.CWD)
	}

	out := Listing{LocalKey: localKey}
	for _, s := range byID {
		info := repos.lookup(s.CWD)
		s.RepoKey, s.RepoRoot = info.Key, info.Root
		if s.CWD != "" {
			if fi, err := os.Stat(s.CWD); err == nil && fi.IsDir() {
				s.DirOK = true
			}
		}
		s.Local = (s.RepoKey != "" && s.RepoKey == localKey) ||
			(s.RepoKey == "" && s.DirOK && canon(s.CWD) == localKey)
		s.Live = liveness[s.ID]
		s.Owner, s.OwnerState = resolveOwner(s.ID, registry, in.Probe, owners, hists, names)
		out.Sessions = append(out.Sessions, s)
	}
	sort.Slice(out.Sessions, func(i, j int) bool {
		return out.Sessions[i].MTime.After(out.Sessions[j].MTime)
	})
	return out
}

// readSession stats and tail-parses one transcript file. The first pass
// reads TailBudget bytes, which resolves every field for almost every
// transcript — but a session can end in one enormous message, starving the
// tail of any munge-verifiable cwd while the file plainly holds one. A row
// wrongly marked unresumable is the picker lying, so the window widens
// geometrically until a cwd verifies or the whole file has been read (rare:
// 5 of 178 in the live store, and only ever on multi-MB files).
func readSession(storeDir, storeDirName, name string) *Session {
	path := filepath.Join(storeDir, name)
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	size := fi.Size()
	var tail Tail
	for window := int64(TailBudget); ; window *= 2 {
		off := size - window
		if off < 0 {
			off = 0
		}
		buf := make([]byte, size-off)
		n, _ := f.ReadAt(buf, off)
		tail = ParseTail(buf[:n], storeDirName, off == 0)
		// Both load-bearing rendered facts, or the whole file: a window that
		// starts inside one enormous assistant record skips it as a partial
		// line, and a later small cwd record must not end the search while
		// the session's only model claim sits unread.
		if (tail.CWD != "" && tail.Model != "") || off == 0 {
			break
		}
	}

	id := tail.ID
	if id == "" {
		// The filename is the fallback identity: the uuid before any suffix
		// (".jsonl", ".orphaned-<ts>-<hash>.jsonl").
		id, _, _ = strings.Cut(name, ".")
	}
	if id == "" {
		return nil
	}
	return &Session{
		ID: id, Path: path, StoreDir: storeDir,
		MTime: fi.ModTime(), Size: size, Tail: tail, CWD: tail.CWD,
	}
}

// resolveOwner is the affinity contract: the owner is the newest account
// claim headroom can observe or was explicitly given — not unobservable
// ground truth. A verified live registry entry outranks everything (that
// account is driving the session at this instant); otherwise the explicit
// re-home record and each account's newest prompt compete on one honest
// axis, event time on this machine's clock. Nothing is ever re-stamped, so
// stale evidence can't be promoted.
func resolveOwner(id string, registry []RegistryEntry, probe PIDProbe,
	owners map[string]OwnerRec, hists map[string]History, names map[string]bool) (string, OwnerState) {
	if acct, ok := LiveAccount(registry, probe, id); ok {
		return acct, OwnerLive
	}

	bestAcct, bestTS, bestState := "", int64(0), OwnerNone
	conflict := false
	consider := func(acct string, ts int64, st OwnerState) {
		switch {
		case ts > bestTS:
			bestAcct, bestTS, bestState = acct, ts, st
			conflict = false
		case ts == bestTS && ts > 0 && acct != bestAcct:
			// Two accounts claiming the same newest moment is undecidable,
			// whatever the sources — the contract has exactly one tie rule,
			// visible fallback, and a re-home exception here would make the
			// spec lie about the rare case it exists to keep honest.
			conflict = true
		}
	}
	if rec, ok := owners[id]; ok {
		if names[rec.Account] {
			consider(rec.Account, rec.AtMS, OwnerRehome)
		} else {
			// The re-homed-to account is gone. Surfacing that beats silently
			// falling through to older history evidence that would resurrect
			// a routing the user explicitly abandoned.
			consider("", rec.AtMS, OwnerMissing)
		}
	}
	for acct, h := range hists {
		if ts, ok := h.Newest[id]; ok {
			consider(acct, ts, OwnerHistory)
		}
	}
	switch {
	case conflict:
		return "", OwnerConflict
	case bestState == OwnerMissing:
		return "", OwnerMissing
	default:
		return bestAcct, bestState
	}
}

// DeleteTranscript removes a session from the store: the transcript, any
// orphan-suffixed siblings, and the closure directory (subagents,
// tool-results, …). The caller is responsible for refusing live sessions;
// this function only refuses ids that could escape the store dir.
func DeleteTranscript(s *Session) error {
	if s.ID == "" || strings.ContainsAny(s.ID, "/.") {
		return os.ErrInvalid
	}
	var firstErr error
	note := func(err error) {
		if err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	note(os.Remove(filepath.Join(s.StoreDir, s.ID+".jsonl")))
	orphans, _ := filepath.Glob(filepath.Join(s.StoreDir, s.ID+".orphaned-*.jsonl"))
	for _, o := range orphans {
		note(os.Remove(o))
	}
	note(os.RemoveAll(filepath.Join(s.StoreDir, s.ID)))
	return firstErr
}
