package sessions_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

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
	st := state.Open(t.TempDir())
	live := func() (map[string]bool, bool) { return sessions.TranscriptIDs(projects) }
	now := time.Now()
	write("-tmp-p", "s-old.jsonl", "", now)

	// A re-home for a session that has since lost its transcript…
	if err := st.ReHome("s-gone", "a@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	// …then s-new appears (created after any earlier listing), is re-homed…
	write("-tmp-p", "s-new.jsonl", "", now)
	if err := st.ReHome("s-new", "b@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	// …and a further write GCs: s-gone (no transcript) goes, s-new stays.
	if err := st.ReHome("s-old", "c@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	m := st.Load().Owners()
	if _, ok := m["s-gone"]; ok {
		t.Error("transcriptless record must be swept")
	}
	if m["s-new"].Account != "b@x.com" || m["s-old"].Account != "c@x.com" {
		t.Errorf("live records must survive every write: %v", m)
	}
}

func TestOwnersGCSkipsOnPartialEnumeration(t *testing.T) {
	projects, write := storeFixture(t)
	st := state.Open(t.TempDir())
	live := func() (map[string]bool, bool) { return sessions.TranscriptIDs(projects) }
	now := time.Now()
	write("-p-hidden", "s-hidden.jsonl", "", now)
	write("-p-open", "s-open.jsonl", "", now)
	if err := st.ReHome("s-hidden", "a@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(projects, "-p-hidden")
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(hidden, 0o755) })
	if err := st.ReHome("s-open", "b@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	m := st.Load().Owners()
	if m["s-hidden"].Account != "a@x.com" {
		t.Errorf("partial enumeration must not GC: %v", m)
	}
}
