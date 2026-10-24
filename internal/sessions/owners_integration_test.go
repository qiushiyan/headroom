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

// TranscriptIDs is the sweep's predicate, so a walk that failed anywhere must
// conclude nothing: a partial set would delete every re-home whose transcript
// sits in the dir it could not read. The sweep itself — gone ones go, young
// and present ones stay — is owned by app's
// TestALaunchThatRecordsASessionSweepsTheGoneOnes, through a real launch.
func TestOwnersGCSkipsOnPartialEnumeration(t *testing.T) {
	projects, write := storeFixture(t)
	st := state.Open(config.Scope{AccountsRoot: t.TempDir()})
	live := func() (map[string]bool, bool) { return sessions.TranscriptIDs(projects) }
	now := time.Now()
	write("-p-hidden", "s-hidden.jsonl", "", now)
	write("-p-open", "s-open.jsonl", "", now)
	// Old enough that the sweep's grace for a transcript not yet written does
	// not cover it: only the refusal to conclude from a partial walk keeps it.
	if err := rehome(st, "s-hidden", "a@x.com", now.Add(-time.Hour), live); err != nil {
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
