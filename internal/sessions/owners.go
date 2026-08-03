package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The .owners file records exactly one kind of fact: the user pressed the
// re-home key on a session, routing it to an account the vendor's own
// evidence doesn't (yet) name. Ordinary resumes write nothing here — the
// launch itself becomes vendor evidence (the live registry immediately, the
// prompt history at the first prompt) — so the file stays small, and its
// timestamps are honest event times that merge cleanly with history
// timestamps: same machine, same clock, same axis ("when did an account
// last drive this session").
//
// A missing or corrupt file is an empty store, like .throttle: losing a
// re-home downgrades routing to derived evidence, visibly, never breaks the
// tool. `check` is where that loss becomes a report instead of a silence.

// OwnerRec is one explicit re-home.
type OwnerRec struct {
	Account string `json:"account"`
	AtMS    int64  `json:"atMs"`
}

type ownersDoc struct {
	Owners map[string]OwnerRec `json:"owners"`
}

// ParseOwners is the one reader of the .owners document, shared by routing
// and by `check` — a checker with its own looser decode once passed a file
// the loader was rejecting wholesale.
func ParseOwners(data []byte) (map[string]OwnerRec, error) {
	var doc ownersDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Owners == nil {
		return nil, errors.New("no owners object")
	}
	return doc.Owners, nil
}

// LoadOwners reads the re-home records; any failure is an empty store.
func LoadOwners(path string) map[string]OwnerRec {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]OwnerRec{}
	}
	m, err := ParseOwners(data)
	if err != nil {
		return map[string]OwnerRec{}
	}
	return m
}

// UpdateOwners applies one mutation under an exclusive lock: reload, mutate,
// GC, write, rename. The lock (a sidecar .lock file, flock'd) is what makes
// read-modify-write safe against a concurrent picker — atomic rename alone
// protects readers from torn files, not writers from each other, and unlike
// a lost throttle claim a lost re-home is a lost user decision.
//
// GC — dropping records whose transcript no longer exists, so the file
// tracks the store instead of growing forever — enumerates the store *here,
// inside the lock*, from projectsDir. It must never be a caller-supplied
// predicate: a predicate is a snapshot of the caller's listing, and a
// concurrent picker's re-home of a session created after that listing would
// be swept away by the very lock that claims to protect it. Empty
// projectsDir skips GC.
func UpdateOwners(path, projectsDir string, mutate func(map[string]OwnerRec)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	m := LoadOwners(path)
	// GC before the mutation, so the record this very call writes can never
	// be swept by it — the sweep only ever removes entries that predate this
	// lock acquisition and lost their transcript.
	if projectsDir != "" {
		if exists, ok := transcriptIDs(projectsDir); ok {
			for id := range m {
				if !exists[id] {
					delete(m, id)
				}
			}
		}
		// A store that failed to enumerate skips GC entirely: an empty or
		// unreadable walk must not mass-delete every re-home.
	}
	mutate(m)
	data, err := json.Marshal(ownersDoc{Owners: m})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".owners-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename lands
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Sync like .current, not like .throttle: this file records a human
	// decision, and losing it to a crash silently un-re-homes a session.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// transcriptIDs enumerates the ids present in the store right now, by
// filename (the uuid before any suffix — canonical and orphaned alike).
// ok=false means the walk itself failed and nothing may be concluded.
func transcriptIDs(projectsDir string) (map[string]bool, bool) {
	dirs, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, false
	}
	ids := map[string]bool{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(projectsDir, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			id, _, _ := strings.Cut(f.Name(), ".")
			if id != "" {
				ids[id] = true
			}
		}
	}
	return ids, true
}

// ReHome records that the user explicitly routed a session to an account.
func ReHome(path, projectsDir, id, account string, at time.Time) error {
	return UpdateOwners(path, projectsDir, func(m map[string]OwnerRec) {
		m[id] = OwnerRec{Account: account, AtMS: at.UnixMilli()}
	})
}

// ForgetOwner drops a session's record — a deleted transcript needs no
// routing preference.
func ForgetOwner(path, id string) error {
	return UpdateOwners(path, "", func(m map[string]OwnerRec) {
		delete(m, id)
	})
}
