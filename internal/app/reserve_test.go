package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
)

func TestAccountsReserve(t *testing.T) {
	f, repo, worktree, outside := reservedFixture(t)
	reserve := func(o ReserveOptions) ran {
		t.Helper()
		return call(func(std Streams) error { return AccountsReserve(std, f.cfg, o) })
	}
	held := func() accounts.Reservations {
		t.Helper()
		r, err := accounts.ReadReservations(f.cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if got := reserve(ReserveOptions{}); got.code != 0 || !strings.Contains(got.stdout, "held@x.com") || !strings.Contains(got.stdout, repo) {
		t.Errorf("listing: %+v", got)
	}

	// A worktree names its repository, a nested directory too, and a directory
	// in no repository is itself. Each is written as a launch will compare it.
	got := reserve(ReserveOptions{Account: "free@x.com", Dirs: []string{filepath.Join(worktree, "apps"), repo, outside}})
	if got.code != 0 {
		t.Fatalf("reserve: %+v", got)
	}
	if dirs := held()["free@x.com"]; len(dirs) != 2 || dirs[0] != repo || dirs[1] != outside {
		t.Errorf("rows = %v", dirs)
	}

	for name, o := range map[string]ReserveOptions{
		"no directories":       {Account: "free@x.com"},
		"--clear with dirs":    {Account: "free@x.com", Dirs: []string{repo}, Clear: true},
		"--clear with no name": {Clear: true},
		"dirs with no name":    {Dirs: []string{repo}},
	} {
		if got := reserve(o); got.code != 2 {
			t.Errorf("%s: exit %d", name, got.code)
		}
	}
	if got := reserve(ReserveOptions{Account: "nobody@x.com", Dirs: []string{repo}}); got.code == 0 {
		t.Error("an account that is not discovered was reserved")
	}
	if got := reserve(ReserveOptions{Account: "free@x.com", Dirs: []string{filepath.Join(outside, "gone")}}); got.code == 0 {
		t.Error("a directory that does not exist was reserved")
	}
	if dirs := held()["free@x.com"]; len(dirs) != 2 {
		t.Errorf("a refusal changed the rows: %v", dirs)
	}

	// A row outlives its account until someone clears it, so --clear goes by
	// name alone.
	if err := accounts.SetReservation(f.cfg, "gone@x.com", []string{repo}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gone@x.com", "free@x.com", "held@x.com"} {
		if got := reserve(ReserveOptions{Account: name, Clear: true}); got.code != 0 {
			t.Errorf("clear %s: %+v", name, got)
		}
	}
	if got := reserve(ReserveOptions{}); !strings.Contains(got.stdout, "no account is reserved") {
		t.Errorf("listing with none: %+v", got)
	}
	if _, err := os.Lstat(f.cfg.ReservedFile()); !os.IsNotExist(err) {
		t.Errorf(".reserved outlived its last row: %v", err)
	}
}

// Seeding is where the retention gap opened: a dir sharing the store with no
// settings of its own, which the vendor then writes and prunes everyone's
// history by. Nothing about sharing config is asked for here.
func TestBareAddLinksTheSettingsThatGovernTheStore(t *testing.T) {
	cfg := claudeScope(t.TempDir(), "qiushi")
	if err := os.MkdirAll(cfg.PrimaryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(cfg.PrimaryDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"cleanupPeriodDays":36500}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.PrimaryDir(), "CLAUDE.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := call(func(std Streams) error { return AccountsAdd(std, cfg, AddOptions{Email: "new@x.com"}) })
	if got.code != 0 {
		t.Fatalf("add: %+v", got)
	}
	dir := filepath.Join(cfg.AccountsRoot, "new@x.com")
	a, err := os.Stat(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.Stat(settings)
	if !os.SameFile(a, b) {
		t.Error("the new dir's settings.json is not the primary's")
	}
	if _, err := os.Lstat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("bare add shared more than the store's settings: %v", err)
	}

	// With nothing to link, the add still works: every account then reads the
	// same default, and check says so.
	bare := claudeScope(t.TempDir(), "qiushi")
	if got := call(func(std Streams) error { return AccountsAdd(std, bare, AddOptions{Email: "new@x.com"}) }); got.code != 0 {
		t.Fatalf("add with no primary settings: %+v", got)
	}
}
