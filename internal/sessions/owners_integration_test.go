package sessions_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// rehome moves a session to an account the way the picker's override does: a
// launch that exists to move it.
func rehome(st *state.Store, id, account string, at time.Time, live state.Enumerator) error {
	_, err := st.Place(state.Launch{
		Candidates: []placement.Candidate{{Name: account, Key: "uuid:" + account}},
		Intent:     placement.Intent{Kind: placement.Forced, Account: account, Reason: "test"},
		Session:    id, MustReHome: true, Live: live, Now: at,
	})
	return err
}

func storeFixture(t *testing.T) (string, func(string, string, string, time.Time)) {
	root := t.TempDir()
	return root, func(dir, name, body string, at time.Time) {
		path := filepath.Join(root, dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
}
func TestOwnersGCReadsStoreAtWriteTime(t *testing.T) {
	projects, write := storeFixture(t)
	st := state.Open(config.Scope{AccountsRoot: t.TempDir()})
	live := func() (map[string]bool, bool) { return sessions.TranscriptIDs(projects) }
	now := time.Now()
	write("-tmp-p", "s-old.jsonl", "", now)

	// A re-home for a session that has since lost its transcript…
	if err := rehome(st, "s-gone", "a@x.com", now.Add(-time.Hour), live); err != nil {
		t.Fatal(err)
	}
	// …and one for a session whose transcript does not exist *yet*: a first
	// turn's id is known before the vendor has written a line of it.
	if err := rehome(st, "s-starting", "d@x.com", now.Add(-time.Minute), live); err != nil {
		t.Fatal(err)
	}
	// …then s-new appears (created after any earlier listing), is re-homed…
	write("-tmp-p", "s-new.jsonl", "", now)
	if err := rehome(st, "s-new", "b@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	// …and a further write GCs: s-gone (no transcript) goes, s-new stays.
	if err := rehome(st, "s-old", "c@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	m := st.Load().Owners()
	if _, ok := m["s-gone"]; ok {
		t.Error("transcriptless record must be swept")
	}
	if m["s-new"].Account != "b@x.com" || m["s-old"].Account != "c@x.com" {
		t.Errorf("live records must survive every write: %v", m)
	}
	if m["s-starting"].Account != "d@x.com" {
		t.Errorf("a record younger than its transcript must not be swept as an orphan: %v", m)
	}
}

func TestOwnersGCSkipsOnPartialEnumeration(t *testing.T) {
	projects, write := storeFixture(t)
	st := state.Open(config.Scope{AccountsRoot: t.TempDir()})
	live := func() (map[string]bool, bool) { return sessions.TranscriptIDs(projects) }
	now := time.Now()
	write("-p-hidden", "s-hidden.jsonl", "", now)
	write("-p-open", "s-open.jsonl", "", now)
	if err := rehome(st, "s-hidden", "a@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(projects, "-p-hidden")
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(hidden, 0o755) })
	if err := rehome(st, "s-open", "b@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	m := st.Load().Owners()
	if m["s-hidden"].Account != "a@x.com" {
		t.Errorf("partial enumeration must not GC: %v", m)
	}
}
