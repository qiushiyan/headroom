package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func clearOverrides(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"HEADROOM_HOME", "HEADROOM_ACCOUNTS_ROOT", "HEADROOM_PRIMARY_NAME", "HEADROOM_LAUNCHER_FORMAT", "HEADROOM_USAGE_URL",
		"HEADROOM_CODEX_ACCOUNTS_ROOT", "HEADROOM_CODEX_PRIMARY_NAME", "HEADROOM_CODEX_LAUNCHER_FORMAT", "HEADROOM_CODEX_USAGE_URL",
	} {
		t.Setenv(v, "")
	}
}

// Relative overrides refuse outright: verified against the 2.1.220 binary, a
// relative CLAUDE_CONFIG_DIR runs the session as the primary (default
// Keychain item) while writing state beside the cwd, and every config-dir
// path headroom builds derives from these roots.
func TestLoadRefusesRelativeOverrides(t *testing.T) {
	for v, rel := range map[string]string{
		"HEADROOM_HOME":                "fixture",
		"HEADROOM_ACCOUNTS_ROOT":       "./accounts",
		"HEADROOM_CODEX_ACCOUNTS_ROOT": "relative/root",
	} {
		clearOverrides(t)
		t.Setenv(v, rel)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), v) {
			t.Errorf("relative %s: err = %v, want a refusal naming it", v, err)
		}
	}
}

// launch.Prepare refuses a primary launch on each scope's own field: a
// primary is selected by the vendor's home variable being absent, which the
// vendor resolves against the real home, not the re-pointed one.
func TestLoadMarksRelocatedPrimary(t *testing.T) {
	clearOverrides(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Claude.PrimaryRelocated || c.Codex.PrimaryRelocated {
		t.Errorf("unset HEADROOM_HOME read as relocated: claude %v codex %v", c.Claude.PrimaryRelocated, c.Codex.PrimaryRelocated)
	}

	t.Setenv("HEADROOM_HOME", t.TempDir())
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if !c.Claude.PrimaryRelocated || !c.Codex.PrimaryRelocated {
		t.Errorf("a re-pointed home must mark both scopes — primary launches refuse on it: claude %v codex %v", c.Claude.PrimaryRelocated, c.Codex.PrimaryRelocated)
	}
}

// Each HEADROOM_* override lands on its own vendor's field and nowhere else.
// Unset, the primary's name is not compiled in — Load leaves it "" so
// discovery derives it from the primary's login — and the advertised launcher
// is a command every install has; a launcher format must carry exactly one
// %s, since a malformed one would render every hint wrong.
func TestLoadOverridesLandOnTheirOwnScope(t *testing.T) {
	clearOverrides(t)
	home := t.TempDir()
	t.Setenv("HEADROOM_HOME", home)
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if base.Claude.PrimaryName != "" || base.Codex.PrimaryName != "" {
		t.Errorf("unset primary names = %q, %q, want \"\" (derive at discovery)", base.Claude.PrimaryName, base.Codex.PrimaryName)
	}
	if base.Claude.LauncherFormat != "headroom launch --account %s" {
		t.Errorf("default LauncherFormat = %q", base.Claude.LauncherFormat)
	}

	root := func(s *Scope) *string { return &s.AccountsRoot }
	name := func(s *Scope) *string { return &s.PrimaryName }
	launcher := func(s *Scope) *string { return &s.LauncherFormat }
	url := func(s *Scope) *string { return &s.UsageURL }
	for _, tc := range []struct {
		env, value string
		vendor     Vendor
		field      func(*Scope) *string
	}{
		{"HEADROOM_ACCOUNTS_ROOT", filepath.Join(home, "claude-root"), Claude, root},
		{"HEADROOM_PRIMARY_NAME", "pinned", Claude, name},
		{"HEADROOM_LAUNCHER_FORMAT", "x-%s", Claude, launcher},
		{"HEADROOM_USAGE_URL", "http://127.0.0.1:1/claude", Claude, url},
		{"HEADROOM_CODEX_ACCOUNTS_ROOT", filepath.Join(home, "codex-root"), Codex, root},
		{"HEADROOM_CODEX_PRIMARY_NAME", "cx", Codex, name},
		{"HEADROOM_CODEX_LAUNCHER_FORMAT", "cx-%s", Codex, launcher},
		{"HEADROOM_CODEX_USAGE_URL", "http://127.0.0.1:1/usage", Codex, url},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			want := base
			*tc.field(map[Vendor]*Scope{Claude: &want.Claude, Codex: &want.Codex}[tc.vendor]) = tc.value
			if !reflect.DeepEqual(c.Claude, want.Claude) || !reflect.DeepEqual(c.Codex, want.Codex) {
				t.Errorf("%s=%q:\n claude %+v\n  codex %+v\nwant only %s's field changed", tc.env, tc.value, c.Claude, c.Codex, tc.vendor)
			}
		})
	}

	for _, env := range []string{"HEADROOM_LAUNCHER_FORMAT", "HEADROOM_CODEX_LAUNCHER_FORMAT"} {
		for _, bad := range []string{"x-", "cx", "x-%d", "%s %s", "%d-%s", "100%% %s"} {
			t.Setenv(env, bad)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), env) {
				t.Errorf("%s=%q: err = %v, want a refusal naming it", env, bad, err)
			}
		}
		t.Setenv(env, "")
	}
}

// Both scopes always resolve — `accounts add --vendor codex` must work before
// any Codex directory exists — and presence alone says whether the board,
// `--json` and `check` include Codex unasked. Either directory makes Codex
// present.
func TestLoadResolvesBothScopes(t *testing.T) {
	clearOverrides(t)
	home := t.TempDir()
	t.Setenv("HEADROOM_HOME", home)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Claude.Present {
		t.Error("Claude Code is always present")
	}
	if c.Codex.Present {
		t.Error("Codex present with neither ~/.codex nor its accounts root")
	}
	if got := c.Present(); len(got) != 1 || got[0].Vendor != Claude {
		t.Errorf("Present() = %v, want Claude Code alone", got)
	}
	x := c.Scope(Codex)
	if x.Vendor != Codex || x.AccountsRoot != filepath.Join(home, ".codex-accounts") ||
		x.PrimaryDir() != filepath.Join(home, ".codex") || x.StoreDir() != filepath.Join(home, ".codex", "sessions") ||
		x.StoreLink() != "sessions" || x.Binary() != "codex" || x.Spacing != DefaultSpacing || !x.PrimaryRelocated {
		t.Errorf("Codex scope = %+v", x)
	}
	if x.LauncherFormat != "headroom launch --vendor codex --account %s" {
		t.Errorf("Codex LauncherFormat = %q", x.LauncherFormat)
	}
	if cl := c.Scope(Claude); cl.StoreDir() != filepath.Join(home, ".claude", "projects") || cl.Binary() != "claude" {
		t.Errorf("Claude scope = %+v", cl)
	}

	for _, dir := range []string{".codex", ".codex-accounts"} {
		t.Run(dir+" alone", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HEADROOM_HOME", home)
			if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if c, err := Load(); err != nil || !c.Codex.Present || len(c.Present()) != 2 || c.Present()[0].Vendor != Claude {
				t.Errorf("%s alone must make Codex present, Claude Code first: %v (%v)", dir, c.Present(), err)
			}
		})
	}
}

// Distinct roots keep the two vendors' .current, .order and state.json apart.
// One location refuses however it is spelled or reached: through a symlink,
// through an alias above several components that do not exist yet, and under
// a spelling that differs only in case on a filesystem that folds it —
// seeding under an accepted pair would otherwise create the shared root the
// refusal exists to prevent.
func TestLoadRefusesAliasedAccountsRoots(t *testing.T) {
	clearOverrides(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for name, roots := range map[string][2]string{
		"same spelling":                        {real, real},
		"different spelling":                   {real, real + "/./"},
		"through a symlink":                    {real, link},
		"absent leaf under a symlinked parent": {filepath.Join(real, "new"), filepath.Join(link, "new")},
		"alias above two missing components":   {filepath.Join(real, "new", "accounts"), filepath.Join(link, "new", "accounts")},
	} {
		t.Setenv("HEADROOM_ACCOUNTS_ROOT", roots[0])
		t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", roots[1])
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "same location") {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	for name, roots := range map[string][2]string{
		"distinct roots": {real, filepath.Join(real, "codex")},
		"distinct siblings above missing components": {filepath.Join(real, "a", "accounts"), filepath.Join(real, "b", "accounts")},
	} {
		t.Setenv("HEADROOM_ACCOUNTS_ROOT", roots[0])
		t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", roots[1])
		c, err := Load()
		if err != nil {
			t.Errorf("%s refused: %v", name, err)
		} else if c.Claude.AccountsRoot != roots[0] {
			t.Errorf("%s: an accepted spelling was rewritten: %q", name, c.Claude.AccountsRoot)
		}
	}

	existing := filepath.Join(real, "Accounts")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", existing)
	t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", filepath.Join(real, "accounts"))
	_, err := Load()
	if _, statErr := os.Stat(filepath.Join(real, "accounts")); statErr == nil {
		// The filesystem folds case: both spellings are one directory.
		if err == nil || !strings.Contains(err.Error(), "same location") {
			t.Errorf("case-folded spellings of one directory: err = %v, want a refusal", err)
		}
	} else if err != nil {
		t.Errorf("distinct directories on a case-sensitive filesystem refused: %v", err)
	}
}

// A home that is not this user's login home spells its primary out: absent,
// CLAUDE_CONFIG_DIR would take config from $HOME/.claude and the credential
// from the base Keychain item, which belongs to the login home's primary.
func TestASecondHomeSpellsItsPrimaryOut(t *testing.T) {
	clearOverrides(t)
	login, second := t.TempDir(), t.TempDir()
	prev := loginHome
	loginHome = func() string { return login }
	t.Cleanup(func() { loginHome = prev })

	t.Setenv("HOME", second)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Claude.PrimaryExplicit || c.Claude.PrimaryRelocated {
		t.Errorf("second home: explicit %v relocated %v, want explicit and not relocated", c.Claude.PrimaryExplicit, c.Claude.PrimaryRelocated)
	}
	if c.Claude.PrimaryDir() != filepath.Join(second, ".claude") || c.Claude.PrimaryMeta() != filepath.Join(second, ".claude", ".claude.json") {
		t.Errorf("second home's primary: dir %s meta %s — spelled out, its .claude.json lives inside it", c.Claude.PrimaryDir(), c.Claude.PrimaryMeta())
	}
	if c.Codex.PrimaryExplicit {
		t.Error("Codex keeps its login in the home: nothing about it is spelled out")
	}

	t.Setenv("HOME", login)
	if c, err = Load(); err != nil || c.Claude.PrimaryExplicit {
		t.Errorf("login home: explicit %v (%v), want selection by absence as always", c.Claude.PrimaryExplicit, err)
	}
	if c.Claude.PrimaryMeta() != filepath.Join(login, ".claude.json") {
		t.Errorf("login home's primary meta = %s", c.Claude.PrimaryMeta())
	}

	// No user database to ask: today's behavior.
	loginHome = func() string { return "" }
	t.Setenv("HOME", second)
	if c, _ = Load(); c.Claude.PrimaryExplicit {
		t.Error("an unknown login home made the primary explicit")
	}
}

// `.ledger` is read strictly: a home that meant to share a ledger and quietly
// kept its own would ask every subscription twice and pile launches onto
// accounts the other home is filling.
func TestTheLedgerFileIsReadStrictly(t *testing.T) {
	clearOverrides(t)
	home, other := t.TempDir(), t.TempDir()
	t.Setenv("HEADROOM_HOME", home)
	root := filepath.Join(home, ".claude-accounts")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(root, ".ledger")

	c, err := Load()
	if err != nil || c.Claude.LedgerRoot != "" || c.Claude.Ledger() != root {
		t.Fatalf("absent: ledger root %q ledger %q err %v", c.Claude.LedgerRoot, c.Claude.Ledger(), err)
	}
	if err := os.WriteFile(ledger, []byte(other+"/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(); err != nil || c.Claude.LedgerRoot != other || c.Claude.Ledger() != other {
		t.Fatalf("another root: ledger root %q ledger %q err %v", c.Claude.LedgerRoot, c.Claude.Ledger(), err)
	}
	if c.Codex.LedgerRoot != "" {
		t.Error("Codex was given the Claude Code ledger")
	}

	for name, content := range map[string]string{
		"empty":      "\n",
		"relative":   "accounts\n",
		"missing":    filepath.Join(other, "gone") + "\n",
		"codex root": filepath.Join(home, ".codex-accounts") + "\n",
		"two lines":  other + "\n" + other + "\n",
	} {
		if name == "codex root" {
			if err := os.MkdirAll(filepath.Join(home, ".codex-accounts"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(ledger, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), ".ledger") {
			t.Errorf("%s: err = %v, want a refusal naming the file", name, err)
		}
	}

	// Naming one's own root is having one's own ledger.
	if err := os.WriteFile(ledger, []byte(root+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(); err != nil || c.Claude.LedgerRoot != "" || c.Claude.Ledger() != root {
		t.Errorf("own root: ledger root %q ledger %q err %v", c.Claude.LedgerRoot, c.Claude.Ledger(), err)
	}
}

// A ledger is a place, not a pointer to follow: a root that itself spends
// against another root's ledger is no ledger, and two homes pointing at each
// other would each spend against the other's file while both read as shared.
func TestALedgerRootMustNotDelegate(t *testing.T) {
	clearOverrides(t)
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("HEADROOM_HOME", a)
	rootA := filepath.Join(a, ".claude-accounts")
	rootB := filepath.Join(b, ".claude-accounts")
	for _, r := range []string{rootA, rootB} {
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootB, ".ledger"), []byte(rootA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, ".ledger"), []byte(rootB+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "itself spends") {
		t.Errorf("a ledger root that delegates was accepted: %v", err)
	}
	// A root whose .ledger names itself keeps its own: still a ledger.
	if err := os.WriteFile(filepath.Join(rootB, ".ledger"), []byte(rootB+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.Claude.LedgerRoot != rootB {
		t.Errorf("a self-naming target: %q %v", c.Claude.LedgerRoot, err)
	}
}
