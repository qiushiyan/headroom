package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// clearOverrides leaves Load nothing but defaults, under a HOME of the test's
// own: unset, HEADROOM_HOME is HOME, and Load would read the user's own
// accounts root's .ledger and ~/.codex.
func clearOverrides(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"HEADROOM_HOME", "HEADROOM_ACCOUNTS_ROOT", "HEADROOM_PRIMARY_NAME", "HEADROOM_LAUNCHER_FORMAT", "HEADROOM_USAGE_URL",
		"HEADROOM_CODEX_ACCOUNTS_ROOT", "HEADROOM_CODEX_PRIMARY_NAME", "HEADROOM_CODEX_LAUNCHER_FORMAT", "HEADROOM_CODEX_USAGE_URL",
	} {
		t.Setenv(v, "")
	}
	t.Setenv("HOME", t.TempDir())
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
	// Where the absent vendor's dirs would be is cmd/headroom's
	// TestCommandsOnAnAbsentVendor, through the add that creates them.
	if x := c.Scope(Codex); x.Vendor != Codex || !x.PrimaryRelocated {
		t.Errorf("Codex scope = %+v", x)
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

// Distinct roots keep the two vendors' .reserved, .order and state.json apart.
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
