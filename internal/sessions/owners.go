package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// tool.

// OwnerRec is one explicit re-home.
type OwnerRec struct {
	Account string `json:"account"`
	AtMS    int64  `json:"atMs"`
}

type ownersDoc struct {
	Owners map[string]OwnerRec `json:"owners"`
}

// LoadOwners reads the re-home records.
func LoadOwners(path string) map[string]OwnerRec {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]OwnerRec{}
	}
	var doc ownersDoc
	if json.Unmarshal(data, &doc) != nil || doc.Owners == nil {
		return map[string]OwnerRec{}
	}
	return doc.Owners
}

// UpdateOwners applies one mutation under an exclusive lock: reload, mutate,
// GC, write, rename. The lock (a sidecar .lock file, flock'd) is what makes
// read-modify-write safe against a concurrent picker — atomic rename alone
// protects readers from torn files, not writers from each other, and unlike
// a lost throttle claim a lost re-home is a lost user decision.
//
// keep reports whether an id's record is still worth holding; records for
// transcripts that no longer exist (and aren't live) are dropped here, on
// write, so the file tracks the store instead of growing forever. keep=nil
// skips GC — correct when the caller can't enumerate the store.
func UpdateOwners(path string, mutate func(map[string]OwnerRec), keep func(id string) bool) error {
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
	mutate(m)
	if keep != nil {
		for id := range m {
			if !keep(id) {
				delete(m, id)
			}
		}
	}
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

// ReHome records that the user explicitly routed a session to an account.
func ReHome(path, id, account string, at time.Time, keep func(string) bool) error {
	return UpdateOwners(path, func(m map[string]OwnerRec) {
		m[id] = OwnerRec{Account: account, AtMS: at.UnixMilli()}
	}, keep)
}

// ForgetOwner drops a session's record — a deleted transcript needs no
// routing preference.
func ForgetOwner(path, id string) error {
	return UpdateOwners(path, func(m map[string]OwnerRec) {
		delete(m, id)
	}, nil)
}
