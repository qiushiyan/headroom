package accounts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	return config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
}

func mkAccount(t *testing.T, cfg config.Config, email string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, email), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverOrderAndGlob(t *testing.T) {
	cfg := testConfig(t)
	for _, e := range []string{"b@x.com", "a@x.com", "c@x.com"} {
		mkAccount(t, cfg, e)
	}
	// .order promotes c then b (with comments, stray spaces, a missing dir,
	// and a duplicate); a follows alphabetically.
	order := "c@x.com\n# a comment\n  b@x.com  \nmissing@x.com\nc@x.com\n"
	if err := os.WriteFile(cfg.OrderFile(), []byte(order), 0o644); err != nil {
		t.Fatal(err)
	}

	accts := Discover(cfg)
	got := make([]string, len(accts))
	for i, a := range accts {
		got[i] = a.Name
	}
	want := []string{"qiushi", "c@x.com", "b@x.com", "a@x.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if !accts[0].IsPrimary() {
		t.Error("first account should be the primary")
	}
}

func TestDiscoverNoRoot(t *testing.T) {
	cfg := testConfig(t)
	accts := Discover(cfg)
	if len(accts) != 1 || !accts[0].IsPrimary() {
		t.Fatalf("expected only the primary, got %v", accts)
	}
}

func TestLauncher(t *testing.T) {
	cfg := testConfig(t)
	mk := func(name string) Account {
		if name == "" {
			return Account{ConfigDir: "", Name: cfg.PrimaryName}
		}
		return Account{ConfigDir: filepath.Join(cfg.AccountsRoot, name), Name: name}
	}
	all := []Account{
		mk(""),
		mk("yan@planlab.ai"),
		mk("dup@x.com"),
		mk("dup@y.com"),
		mk("select@x.com"),
		mk("qiushi@z.com"),
		mk("noatsign"),
	}
	cases := []struct {
		name string
		want string
	}{
		{"", "x-qiushi"},                   // primary
		{"yan@planlab.ai", "x-yan"},        // unique local part → short alias
		{"dup@x.com", "x-dup@x.com"},       // ambiguous local part → full email
		{"dup@y.com", "x-dup@y.com"},       // ambiguous local part → full email
		{"select@x.com", "x-select@x.com"}, // reserved utility name → full email
		{"qiushi@z.com", "x-qiushi@z.com"}, // primary's name → full email
		{"noatsign", "x-noatsign"},         // no @ → the name is the email
	}
	for _, c := range cases {
		if got := Launcher(mk(c.name), all, cfg.PrimaryName); got != c.want {
			t.Errorf("Launcher(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCurrentTarget(t *testing.T) {
	cfg := testConfig(t)
	if got := CurrentTarget(cfg); got != "qiushi" {
		t.Errorf("no state file: got %q, want primary", got)
	}
	if err := SetCurrent(cfg, "yan@planlab.ai"); err != nil {
		t.Fatal(err)
	}
	if got := CurrentTarget(cfg); got != "yan@planlab.ai" {
		t.Errorf("got %q, want yan@planlab.ai", got)
	}
	if err := os.WriteFile(cfg.StateFile(), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CurrentTarget(cfg); got != "qiushi" {
		t.Errorf("empty state file: got %q, want primary", got)
	}
}

func TestSetCurrentAtomicWrite(t *testing.T) {
	cfg := testConfig(t)
	if err := SetCurrent(cfg, "first@x.com"); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrent(cfg, "second@x.com"); err != nil {
		t.Fatal(err)
	}
	if got := CurrentTarget(cfg); got != "second@x.com" {
		t.Errorf("got %q, want second@x.com", got)
	}
	// The write goes through a temp file + rename; nothing may be left over.
	entries, err := os.ReadDir(cfg.AccountsRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".current" {
			t.Errorf("stray file after SetCurrent: %q", e.Name())
		}
	}
}

func TestMetaEmail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	if _, ok := MetaEmail(path); ok {
		t.Error("missing file should not be ok")
	}
	os.WriteFile(path, []byte(`not json`), 0o644)
	if _, ok := MetaEmail(path); ok {
		t.Error("invalid json should not be ok")
	}
	os.WriteFile(path, []byte(`{"oauthAccount":{}}`), 0o644)
	if _, ok := MetaEmail(path); ok {
		t.Error("missing field should not be ok")
	}
	os.WriteFile(path, []byte(`{"oauthAccount":{"emailAddress":"a@b.c"}}`), 0o644)
	email, ok := MetaEmail(path)
	if !ok || email != "a@b.c" {
		t.Errorf("got %q ok=%v, want a@b.c true", email, ok)
	}
}
