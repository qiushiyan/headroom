package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

func TestSeedShape(t *testing.T) {
	cfg := testConfig(t)
	dir, shared, err := Seed(cfg, "new@x.com", SeedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(cfg.AccountsRoot, "new@x.com") || len(shared) != 0 {
		t.Fatalf("dir=%q shared=%v", dir, shared)
	}
	// The store was created as a real dir and projects/ links to it.
	if fi, err := os.Lstat(cfg.StoreDir()); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("canonical store not a real directory: %v %v", fi, err)
	}
	if err := VerifyTopology(Account{Scope: cfg, ConfigDir: dir, Name: "new@x.com"}); err != nil {
		t.Fatalf("seeded dir fails topology: %v", err)
	}
	// Discovery lists it; the strict selector resolves it.
	if _, err := Discover(cfg).Select("new@x.com"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Seeding twice refuses.
	if _, _, err := Seed(cfg, "new@x.com", SeedOptions{}); err == nil {
		t.Fatal("second seed should refuse")
	}
}

func TestSeedRefusesBadNames(t *testing.T) {
	cfg := testConfig(t)
	for _, n := range []string{"", "noatsign", "a@x.com.lock", "../a@x.com", "sub/a@x.com"} {
		if _, _, err := Seed(cfg, n, SeedOptions{}); err == nil {
			t.Errorf("Seed(%q) accepted", n)
		}
	}
	// Nothing was created for any of them.
	if entries, _ := os.ReadDir(cfg.AccountsRoot); len(entries) != 0 {
		t.Errorf("accounts root not empty: %v", entries)
	}
}

func TestSeedRefusesLinkedStore(t *testing.T) {
	cfg := testConfig(t)
	real := filepath.Join(cfg.Home, "elsewhere")
	os.MkdirAll(real, 0o755)
	os.MkdirAll(cfg.PrimaryDir(), 0o755)
	os.Symlink(real, cfg.StoreDir())
	if _, _, err := Seed(cfg, "a@x.com", SeedOptions{}); err == nil {
		t.Fatal("a symlinked canonical store must refuse")
	}
	if _, err := os.Lstat(filepath.Join(cfg.AccountsRoot, "a@x.com")); err == nil {
		t.Fatal("dir was created despite the refusal")
	}
}

func TestSeedShareWhitelist(t *testing.T) {
	cfg := testConfig(t)
	// The primary dir holds config and state; only the whitelist is shared.
	os.MkdirAll(filepath.Join(cfg.PrimaryDir(), "skills"), 0o755)
	os.MkdirAll(filepath.Join(cfg.PrimaryDir(), "sessions"), 0o755)
	os.WriteFile(filepath.Join(cfg.PrimaryDir(), "settings.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(cfg.PrimaryDir(), "history.jsonl"), []byte(""), 0o644)
	dir, shared, err := Seed(cfg, "a@x.com", SeedOptions{ShareFrom: cfg.PrimaryDir(), ShareNames: SharedConfigEntries(cfg.Vendor)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"settings.json": true, "skills": true}
	if len(shared) != 2 || !want[shared[0]] || !want[shared[1]] {
		t.Fatalf("shared = %v, want settings.json and skills only", shared)
	}
	for _, n := range []string{"history.jsonl", "sessions"} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s was shared — per-account state must stay per account", n)
		}
	}
	if target, _ := os.Readlink(filepath.Join(dir, "settings.json")); target != filepath.Join(cfg.PrimaryDir(), "settings.json") {
		t.Errorf("settings.json → %q", target)
	}
}

func TestSeedShareWholeDir(t *testing.T) {
	cfg := testConfig(t)
	pkg := filepath.Join(cfg.Home, "dotfiles", "claude", ".claude")
	os.MkdirAll(filepath.Join(pkg, "hooks"), 0o755)
	os.WriteFile(filepath.Join(pkg, "settings.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(pkg, "anything.md"), []byte(""), 0o644)
	os.Mkdir(filepath.Join(pkg, "projects"), 0o755) // must never be shared over the store link
	dir, shared, err := Seed(cfg, "a@x.com", SeedOptions{ShareFrom: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 3 {
		t.Fatalf("shared = %v, want every entry but projects", shared)
	}
	if err := VerifyTopology(Account{Scope: cfg, ConfigDir: dir, Name: "a@x.com"}); err != nil {
		t.Fatalf("projects link was shadowed: %v", err)
	}
	// Relative and missing sources refuse before anything is made.
	if _, _, err := Seed(cfg, "b@x.com", SeedOptions{ShareFrom: "relative"}); err == nil {
		t.Error("relative ShareFrom accepted")
	}
	if _, _, err := Seed(cfg, "b@x.com", SeedOptions{ShareFrom: filepath.Join(cfg.Home, "nope")}); err == nil {
		t.Error("missing ShareFrom accepted")
	}
	if _, err := os.Lstat(filepath.Join(cfg.AccountsRoot, "b@x.com")); err == nil {
		t.Error("dir created despite refusal")
	}
}

func TestRemoveDirKeepsStoreAndScrubsOrder(t *testing.T) {
	cfg := testConfig(t)
	dir, _, err := Seed(cfg, "a@x.com", SeedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A session in the store, and a private file in the account dir.
	os.WriteFile(filepath.Join(cfg.StoreDir(), "t.jsonl"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte("x"), 0o644)
	os.WriteFile(cfg.OrderFile(), []byte("# order\nb@x.com\na@x.com   \nc@x.com\n"), 0o644)
	os.WriteFile(cfg.CurrentFile(), []byte("a@x.com\n"), 0o644)

	if err := RemoveDir(cfg, "a@x.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Fatal("dir still exists")
	}
	if _, err := os.Stat(filepath.Join(cfg.StoreDir(), "t.jsonl")); err != nil {
		t.Fatal("removing an account deleted the canonical store's contents")
	}
	if got, _ := os.ReadFile(cfg.OrderFile()); string(got) != "# order\nb@x.com\nc@x.com\n" {
		t.Errorf(".order = %q", got)
	}
	if got, _ := os.ReadFile(cfg.CurrentFile()); string(got) != "a@x.com\n" {
		t.Errorf(".current was rewritten to %q — it must be left for the board to repick", got)
	}
	// Lock debris is removable; a symlinked dir and bad names refuse.
	os.Mkdir(filepath.Join(cfg.AccountsRoot, "a@x.com.lock"), 0o755)
	if err := RemoveDir(cfg, "a@x.com.lock"); err != nil {
		t.Errorf("lock debris: %v", err)
	}
	os.Symlink(cfg.PrimaryDir(), filepath.Join(cfg.AccountsRoot, "l@x.com"))
	if err := RemoveDir(cfg, "l@x.com"); err == nil {
		t.Error("symlinked account dir removed")
	}
	for _, n := range []string{"", "../a@x.com", "noatsign", "missing@x.com"} {
		if err := RemoveDir(cfg, n); err == nil {
			t.Errorf("RemoveDir(%q) succeeded", n)
		}
	}
}

// A removed account's projects/ is normally the store link. When it is a
// real directory it holds sessions nobody migrated, and RemoveAll would
// delete them silently — the one irreversible loss removal could cause.
func TestRemoveDirRefusesUnmigratedProjects(t *testing.T) {
	cfg := testConfig(t)
	dir := filepath.Join(cfg.AccountsRoot, "a@x.com")
	os.MkdirAll(filepath.Join(dir, "projects", "p"), 0o755)
	os.WriteFile(filepath.Join(dir, "projects", "p", "t.jsonl"), []byte("x"), 0o644)
	if err := CheckRemovable(cfg, "a@x.com"); err == nil {
		t.Error("CheckRemovable accepted a dir with a real projects/ directory")
	}
	if err := RemoveDir(cfg, "a@x.com"); err == nil {
		t.Fatal("RemoveDir deleted a dir with a real projects/ directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "projects", "p", "t.jsonl")); err != nil {
		t.Fatal("unmigrated session was deleted")
	}
	// A missing projects/ (partial seed) and a link elsewhere are still removable.
	os.RemoveAll(filepath.Join(dir, "projects"))
	if err := RemoveDir(cfg, "a@x.com"); err != nil {
		t.Errorf("partial seed: %v", err)
	}
}

// Obligation 10: Seed for Codex builds what VerifyTopology accepts — on a
// tree where no Codex directory exists yet, and on one where the store does —
// and refuses a store that is a symlink or a file.
func TestSeedCodex(t *testing.T) {
	t.Run("bootstraps an absent tree", func(t *testing.T) {
		cfg := config.ForHome(t.TempDir()).Codex
		if _, err := os.Stat(cfg.PrimaryDir()); err == nil {
			t.Fatal("fixture already has a Codex home")
		}
		dir, _, err := Seed(cfg, "new@x.com", SeedOptions{})
		if err != nil {
			t.Fatalf("Seed on a machine without Codex: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(dir, "sessions")); err != nil || target != cfg.StoreDir() {
			t.Errorf("sessions link = %q, %v; want %s", target, err, cfg.StoreDir())
		}
		if fi, err := os.Lstat(cfg.StoreDir()); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("canonical store not created as a real directory: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "projects")); err == nil {
			t.Error("a Codex home was given Claude Code's projects link")
		}
		set := Discover(cfg)
		a, err := set.Select("new@x.com")
		if err != nil {
			t.Fatalf("the seeded home is not discoverable: %v", err)
		}
		if err := VerifyTopology(a); err != nil {
			t.Errorf("seeded home fails the launch gate: %v", err)
		}
	})
	t.Run("store already exists", func(t *testing.T) {
		cfg := config.ForHome(t.TempDir()).Codex
		if err := os.MkdirAll(cfg.StoreDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(cfg.StoreDir(), "keep")
		os.WriteFile(marker, []byte("x"), 0o644)
		if _, _, err := Seed(cfg, "a@x.com", SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Error("seeding disturbed an existing store")
		}
	})
	t.Run("store is a symlink", func(t *testing.T) {
		cfg := config.ForHome(t.TempDir()).Codex
		os.MkdirAll(cfg.PrimaryDir(), 0o755)
		if err := os.Symlink(t.TempDir(), cfg.StoreDir()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Seed(cfg, "a@x.com", SeedOptions{}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("err = %v, want a refusal naming the symlinked store", err)
		}
	})
	t.Run("store is a file", func(t *testing.T) {
		cfg := config.ForHome(t.TempDir()).Codex
		os.MkdirAll(cfg.PrimaryDir(), 0o755)
		os.WriteFile(cfg.StoreDir(), []byte("x"), 0o644)
		if _, _, err := Seed(cfg, "a@x.com", SeedOptions{}); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("err = %v, want a refusal", err)
		}
	})
	t.Run("an unreadable store is not an absent one", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads everything")
		}
		cfg := config.ForHome(t.TempDir()).Codex
		os.MkdirAll(cfg.PrimaryDir(), 0o755)
		os.Chmod(cfg.PrimaryDir(), 0o000)
		t.Cleanup(func() { os.Chmod(cfg.PrimaryDir(), 0o755) })
		_, _, err := Seed(cfg, "a@x.com", SeedOptions{})
		if err == nil || !strings.Contains(err.Error(), "unreadable") {
			t.Errorf("err = %v, want the unreadable-store refusal, not a fresh store", err)
		}
	})
	t.Run("shares only whitelisted config", func(t *testing.T) {
		cfg := config.ForHome(t.TempDir()).Codex
		for _, n := range []string{"config.toml", "AGENTS.md", "auth.json", "history.jsonl", "state_5.sqlite"} {
			os.MkdirAll(cfg.PrimaryDir(), 0o755)
			os.WriteFile(filepath.Join(cfg.PrimaryDir(), n), []byte("x"), 0o644)
		}
		for _, n := range []string{"skills", "sessions", "prompts"} {
			os.MkdirAll(filepath.Join(cfg.PrimaryDir(), n), 0o755)
		}
		dir, shared, err := Seed(cfg, "a@x.com", SeedOptions{ShareFrom: cfg.PrimaryDir(), ShareNames: SharedConfigEntries(cfg.Vendor)})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(shared, ","); got != "config.toml,AGENTS.md,skills,prompts" {
			t.Errorf("shared = %s", got)
		}
		for _, n := range []string{"auth.json", "history.jsonl", "state_5.sqlite"} {
			if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
				t.Errorf("%s was shared: a login or per-home state must never be", n)
			}
		}
	})
}

// A Codex account cannot be recorded as Claude Code's current account, or the
// reverse: SetCurrent refuses the wrong pairing.
func TestSetCurrentRefusesTheOtherVendorsAccount(t *testing.T) {
	cfg := config.ForHome(t.TempDir())
	codexAcct := Account{Scope: cfg.Codex, Name: "a@x.com"}
	if err := Discover(cfg.Claude).SetCurrent(codexAcct); err == nil {
		t.Error("a Codex account was written into Claude Code's .current")
	}
	if _, err := os.Stat(cfg.Claude.CurrentFile()); err == nil {
		t.Error(".current was written through a refused pairing")
	}
}
