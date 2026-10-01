package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Relative overrides refuse outright: verified against the 2.1.220 binary, a
// relative CLAUDE_CONFIG_DIR runs the session as the primary (default
// Keychain item) while writing state beside the cwd, and every config-dir
// path headroom builds derives from these roots.
func TestLoadRefusesRelativeOverrides(t *testing.T) {
	t.Setenv("HEADROOM_HOME", "fixture")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HEADROOM_HOME") {
		t.Errorf("relative HEADROOM_HOME: err = %v, want a refusal naming it", err)
	}
	t.Setenv("HEADROOM_HOME", "")

	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "./accounts")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HEADROOM_ACCOUNTS_ROOT") {
		t.Errorf("relative HEADROOM_ACCOUNTS_ROOT: err = %v, want a refusal naming it", err)
	}
}

func TestLoadMarksRelocatedPrimary(t *testing.T) {
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "")
	t.Setenv("HEADROOM_HOME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrimaryRelocated {
		t.Error("unset HEADROOM_HOME must not read as relocated")
	}

	fixture := t.TempDir()
	t.Setenv("HEADROOM_HOME", fixture)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PrimaryRelocated {
		t.Error("a re-pointed home must be marked: primary launches refuse on it")
	}
	if cfg.Claude.AccountsRoot != filepath.Join(fixture, ".claude-accounts") {
		t.Errorf("AccountsRoot = %q, want under the fixture home", cfg.Claude.AccountsRoot)
	}
}

// The primary's name is not compiled in: unset, Load leaves it "" so
// discovery derives it from the primary's login; the variable pins it.
func TestLoadPrimaryNameIsDerivedUnlessPinned(t *testing.T) {
	t.Setenv("HEADROOM_HOME", "")
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "")
	t.Setenv("HEADROOM_PRIMARY_NAME", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Claude.PrimaryName != "" {
		t.Errorf("unset HEADROOM_PRIMARY_NAME: PrimaryName = %q, want \"\" (derive at discovery)", c.Claude.PrimaryName)
	}
	t.Setenv("HEADROOM_PRIMARY_NAME", "pinned")
	c, _ = Load()
	if c.Claude.PrimaryName != "pinned" {
		t.Errorf("pinned: PrimaryName = %q", c.Claude.PrimaryName)
	}
}

// The advertised launcher spelling defaults to a command every install has
// and is re-spelled by HEADROOM_LAUNCHER_FORMAT, which must carry exactly
// one %s — a malformed format would render every hint wrong.
func TestLoadLauncherFormat(t *testing.T) {
	t.Setenv("HEADROOM_HOME", "")
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", "")
	t.Setenv("HEADROOM_LAUNCHER_FORMAT", "")
	c, err := Load()
	if err != nil || c.Claude.LauncherFormat != "headroom launch --account %s" {
		t.Errorf("default LauncherFormat = %q, %v", c.Claude.LauncherFormat, err)
	}
	t.Setenv("HEADROOM_LAUNCHER_FORMAT", "x-%s")
	if c, err = Load(); err != nil || c.Claude.LauncherFormat != "x-%s" {
		t.Errorf("pinned LauncherFormat = %q, %v", c.Claude.LauncherFormat, err)
	}
	for _, bad := range []string{"x-", "%s %s", "%d-%s", "100%% %s"} {
		t.Setenv("HEADROOM_LAUNCHER_FORMAT", bad)
		if _, err := Load(); err == nil {
			t.Errorf("HEADROOM_LAUNCHER_FORMAT=%q accepted", bad)
		}
	}
}

func clearOverrides(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"HEADROOM_HOME", "HEADROOM_ACCOUNTS_ROOT", "HEADROOM_PRIMARY_NAME", "HEADROOM_LAUNCHER_FORMAT", "HEADROOM_USAGE_URL",
		"HEADROOM_CODEX_ACCOUNTS_ROOT", "HEADROOM_CODEX_PRIMARY_NAME", "HEADROOM_CODEX_LAUNCHER_FORMAT", "HEADROOM_CODEX_USAGE_URL",
	} {
		t.Setenv(v, "")
	}
}

// Both scopes always resolve — `accounts add --vendor codex` must work before
// any Codex directory exists — and presence alone says whether the board,
// `--json` and `check` include Codex unasked.
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

	// Either directory makes Codex present.
	if err := os.MkdirAll(filepath.Join(home, ".codex-accounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c, _ = Load(); !c.Codex.Present || len(c.Present()) != 2 || c.Present()[0].Vendor != Claude {
		t.Errorf("accounts root alone must make Codex present, Claude Code first: %v", c.Present())
	}
}

func TestLoadCodexOverrides(t *testing.T) {
	clearOverrides(t)
	t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", "relative/root")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HEADROOM_CODEX_ACCOUNTS_ROOT") {
		t.Errorf("relative Codex accounts root: err = %v, want a refusal naming it", err)
	}
	root := t.TempDir()
	t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", root)
	t.Setenv("HEADROOM_CODEX_PRIMARY_NAME", "cx")
	t.Setenv("HEADROOM_CODEX_LAUNCHER_FORMAT", "cx-%s")
	t.Setenv("HEADROOM_CODEX_USAGE_URL", "http://127.0.0.1:1/usage")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Codex.AccountsRoot != root || c.Codex.PrimaryName != "cx" || c.Codex.LauncherFormat != "cx-%s" || c.Codex.UsageURL != "http://127.0.0.1:1/usage" {
		t.Errorf("Codex overrides not applied: %+v", c.Codex)
	}
	if c.Claude.PrimaryName != "" || c.Claude.LauncherFormat != "headroom launch --account %s" {
		t.Errorf("a Codex override leaked into Claude Code's scope: %+v", c.Claude)
	}
	t.Setenv("HEADROOM_CODEX_LAUNCHER_FORMAT", "cx")
	if _, err := Load(); err == nil {
		t.Error("a Codex launcher format without its placeholder was accepted")
	}
}

// Distinct roots keep the two vendors' .current, .order and state.json apart.
// One location refuses however it is spelled.
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
	} {
		t.Setenv("HEADROOM_ACCOUNTS_ROOT", roots[0])
		t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", roots[1])
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "same location") {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", real)
	t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", filepath.Join(real, "codex"))
	c, err := Load()
	if err != nil {
		t.Fatalf("distinct roots refused: %v", err)
	}
	if c.Claude.AccountsRoot != real {
		t.Errorf("an accepted spelling was rewritten: %q", c.Claude.AccountsRoot)
	}
}

// One location must refuse however it is reached: through an alias above
// several components that do not exist yet, and under a spelling that differs
// only in case on a filesystem that folds it. Seeding under an accepted pair
// would otherwise create the shared root the refusal exists to prevent.
func TestLoadRefusesAliasedRootsAboveMissingComponentsAndByCase(t *testing.T) {
	clearOverrides(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", filepath.Join(real, "new", "accounts"))
	t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", filepath.Join(link, "new", "accounts"))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "same location") {
		t.Errorf("alias above two missing components: err = %v, want a refusal", err)
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

	// Distinct siblings above missing components stay accepted.
	t.Setenv("HEADROOM_ACCOUNTS_ROOT", filepath.Join(real, "a", "accounts"))
	t.Setenv("HEADROOM_CODEX_ACCOUNTS_ROOT", filepath.Join(real, "b", "accounts"))
	if _, err := Load(); err != nil {
		t.Errorf("distinct roots refused: %v", err)
	}
}

// Either directory makes Codex present — the primary home alone included.
func TestCodexPresentFromItsPrimaryHomeAlone(t *testing.T) {
	clearOverrides(t)
	home := t.TempDir()
	t.Setenv("HEADROOM_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || !c.Codex.Present {
		t.Errorf("~/.codex alone: present=%v err=%v", c.Codex.Present, err)
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
	if err != nil || c.Claude.LedgerRoot != "" || c.Claude.Ledger() != root || c.Claude.SharesLedger() {
		t.Fatalf("absent: %+v %v", c.Claude, err)
	}
	if err := os.WriteFile(ledger, []byte(other+"/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(); err != nil || c.Claude.LedgerRoot != other || !c.Claude.SharesLedger() {
		t.Fatalf("another root: ledger %q shared %v err %v", c.Claude.LedgerRoot, c.Claude.SharesLedger(), err)
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
	if c, err = Load(); err != nil || c.Claude.SharesLedger() {
		t.Errorf("own root: shared %v err %v", c.Claude.SharesLedger(), err)
	}
}
