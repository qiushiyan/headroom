package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Who may be chosen is each caller's own policy over shared facts. A headless
// placement spends a token on a session its caller starts, not a launch
// headroom runs, so what only a launch needs says nothing about it: an account
// whose dir a launch refuses — its projects link a real directory, the
// history fork — is still a headless candidate. Building headless candidates
// from the launch's eligibility must fail this test.
func TestAHeadlessPlacementIgnoresWhatOnlyALaunchNeeds(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observe("a@x.com", 0, 0, time.Minute)
	f.observe("b@x.com", 30, 30, time.Minute)
	f.mint("a@x.com", 300*24*time.Hour)
	f.mint("b@x.com", 300*24*time.Hour)
	link := filepath.Join(f.cfg.AccountsRoot, "a@x.com", "projects")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}

	doc, code, errw := f.place("--dry-run")
	if a := doc.find("a@x.com"); code != exitPlaced || doc.Account != "a@x.com" || !a.Eligible {
		t.Fatalf("code %d placed on %q, a@x.com %+v: %s", code, doc.Account, a, errw)
	}
}
