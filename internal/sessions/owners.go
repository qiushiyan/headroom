package sessions

import (
	"os"
	"path/filepath"
	"strings"
)

// OwnerRec is one explicit re-home: the user pressed the override key on a
// session, routing it to an account the vendor's own evidence doesn't (yet)
// name. Ordinary resumes record nothing — the launch itself becomes vendor
// evidence (the live registry immediately, the prompt history at the first
// prompt) — so these stay rare, and their timestamps are honest event times
// that merge cleanly with history timestamps: same machine, same clock, same
// axis ("when did an account last drive this session").
//
// Where they are stored is not this package's business. The collector is
// handed the records; internal/state owns reading and writing them, because
// losing a re-home loses a human decision and that needs a lock, not a parser.
type OwnerRec struct {
	Account string
	AtMS    int64
}

// TranscriptIDs enumerates the session ids present in the store right now, by
// filename (the uuid before any suffix — canonical and orphaned alike).
//
// ok=false means the walk failed *anywhere* and nothing may be concluded: a
// partial set that silently omitted one unreadable project dir would, used as
// a garbage-collection predicate, delete every re-home whose transcript lives
// there.
//
// It is passed to the store as a function rather than called ahead of one, so
// it runs inside the lock that protects the records it is used to sweep: a set
// gathered beforehand cannot see a session another picker re-homed since, and
// sweeping against it would delete that re-home.
func TranscriptIDs(projectsDir string) (map[string]bool, bool) {
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
			return nil, false
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
