package accounts

import (
	"os"
	"path/filepath"
	"testing"
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
	if fi, err := os.Lstat(cfg.ProjectsDir()); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("canonical store not a real directory: %v %v", fi, err)
	}
	if err := VerifyTopology(cfg, Account{ConfigDir: dir, Name: "new@x.com"}); err != nil {
		t.Fatalf("seeded dir fails topology: %v", err)
	}
	// Discovery lists it; the strict selector resolves it.
	if _, err := Select(cfg, Discover(cfg), "new@x.com"); err != nil {
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
	os.Symlink(real, cfg.ProjectsDir())
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
	dir, shared, err := Seed(cfg, "a@x.com", SeedOptions{ShareFrom: cfg.PrimaryDir(), ShareNames: SharedConfigEntries})
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
	if err := VerifyTopology(cfg, Account{ConfigDir: dir, Name: "a@x.com"}); err != nil {
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
	os.WriteFile(filepath.Join(cfg.ProjectsDir(), "t.jsonl"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte("x"), 0o644)
	os.WriteFile(cfg.OrderFile(), []byte("# order\nb@x.com\na@x.com   \nc@x.com\n"), 0o644)
	os.WriteFile(cfg.CurrentFile(), []byte("a@x.com\n"), 0o644)

	if err := RemoveDir(cfg, "a@x.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Fatal("dir still exists")
	}
	if _, err := os.Stat(filepath.Join(cfg.ProjectsDir(), "t.jsonl")); err != nil {
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
