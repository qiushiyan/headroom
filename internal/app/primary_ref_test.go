package app

import (
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// The primary's name is display. What a run records under one pin — a
// session's routing, a reservation — is the primary's under any
// other, so a shell started before a rename and one started after read one
// set of records and nothing moves. Recording the primary by its name must
// fail this.
func TestARenamedPrimaryKeepsItsRecords(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.observe("qiushi", 0, 0, time.Minute)
	f.observe("a@x.com", 60, 60, time.Minute)
	if got := f.launch("--", "-p", "--session-id", sessA, "hi"); got.account(f.cfg) != "qiushi" {
		t.Fatalf("first turn: %s", got)
	}
	work := t.TempDir()
	if r := call(func(std Streams) error {
		return AccountsReserve(std, f.cfg, ReserveOptions{Account: "qiushi", Dirs: []string{work}})
	}); r.code != 0 {
		t.Fatalf("reserve: %d %s", r.code, r.stderr)
	}

	renamed := f.cfg
	renamed.PrimaryName = "q2"
	set := accounts.Discover(renamed)
	if owner := sessions.OwnerOf(sessA, launchop.Refs(set), sessions.ProcessEvidence{}, state.Open(renamed).Load().Owners()); owner != "q2" {
		t.Errorf("the session's owner after the rename: %q", owner)
	}
	if keptFrom(t, renamed, "q2", work) || !keptFrom(t, renamed, "q2", t.TempDir()) {
		t.Error("the reservation did not stay with the primary")
	}
}

// keptFrom says a bare launch from dir leaves the account named name out
// because a reservation keeps it for other work.
func keptFrom(t *testing.T, scope config.Scope, name, dir string) bool {
	t.Helper()
	t.Chdir(dir)
	for line := range strings.SplitSeq(launchOn(t, scope, quietPlace(), "--dry-run").stdout, "\n") {
		if strings.Contains(line, name) && strings.Contains(line, "excluded: reserved for") {
			return true
		}
	}
	return false
}
