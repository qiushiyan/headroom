package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// clearOverrides leaves Load nothing but defaults, under a HOME of the test's
// own: unset, HEADROOM_HOME is HOME, and Load would read the user's own
// ~/.codex.
func clearOverrides(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"HEADROOM_HOME", "HEADROOM_PRIMARY_NAME", "HEADROOM_LAUNCHER_FORMAT", "HEADROOM_USAGE_URL",
		"HEADROOM_CODEX_PRIMARY_NAME", "HEADROOM_CODEX_LAUNCHER_FORMAT", "HEADROOM_CODEX_USAGE_URL",
		"HEADROOM_PEERS",
	} {
		t.Setenv(v, "")
	}
	t.Setenv("HOME", t.TempDir())
}

// A relative HEADROOM_HOME refuses outright: verified against the 2.1.220
// binary, a relative CLAUDE_CONFIG_DIR runs the session as the primary
// (default Keychain item) while writing state beside the cwd, and every
// config-dir path headroom builds derives from that home.
func TestLoadRefusesARelativeHome(t *testing.T) {
	clearOverrides(t)
	t.Setenv("HEADROOM_HOME", "fixture")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HEADROOM_HOME") {
		t.Errorf("relative HEADROOM_HOME: err = %v, want a refusal naming it", err)
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

	name := func(s *Scope) *string { return &s.PrimaryName }
	launcher := func(s *Scope) *string { return &s.LauncherFormat }
	url := func(s *Scope) *string { return &s.UsageURL }
	for _, tc := range []struct {
		env, value string
		vendor     Vendor
		field      func(*Scope) *string
	}{
		{"HEADROOM_PRIMARY_NAME", "pinned", Claude, name},
		{"HEADROOM_LAUNCHER_FORMAT", "x-%s", Claude, launcher},
		{"HEADROOM_USAGE_URL", "http://127.0.0.1:1/claude", Claude, url},
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

// Antigravity's scope: agy's state dir is the primary and decides presence,
// its accounts root holds headroom's state file alone, and its reads are
// spaced by its own quiet period. Nothing of it launches, so a relocated home
// refuses nothing of it.
func TestAgyScope(t *testing.T) {
	clearOverrides(t)
	home := t.TempDir()
	t.Setenv("HEADROOM_HOME", home)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Scope(Agy)
	if s.Vendor != Agy || s.PrimaryDir() != filepath.Join(home, ".gemini", "antigravity-cli") ||
		s.AccountsRoot != filepath.Join(home, ".agy-accounts") || s.Binary() != "agy" || s.RequestSpacing() != AgySpacing {
		t.Errorf("scope %+v", s)
	}
	if s.Present || !strings.Contains(fmt.Sprint(s.RequirePresent()), "run it once to sign in") {
		t.Errorf("absent agy: present %v, refusal %v", s.Present, s.RequirePresent())
	}
	if s.PrimaryLaunchRefused() {
		t.Error("a relocated home refuses an Antigravity launch, but there is none")
	}
	// The accounts root alone is headroom's own doing, not agy having run.
	if err := os.MkdirAll(s.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(); cfg.Agy.Present {
		t.Error("the accounts root alone made Antigravity present")
	}
	if err := os.MkdirAll(s.PrimaryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(); !cfg.Agy.Present || len(cfg.Present()) != 2 || cfg.Present()[1].Vendor != Agy {
		t.Errorf("present: %+v", cfg.Present())
	}
	if Agy.Title() != "Antigravity" || Agy.EnvPolicy().HomeVar != "" {
		t.Errorf("title %q, home variable %q", Agy.Title(), Agy.EnvPolicy().HomeVar)
	}
}

// Readings from other machines are opt-in: unset, a scope names no peer and
// nothing is pulled. Set, Claude Code and Codex take the machines named, each
// once, and Antigravity none — each machine reads its own sign-in every time.
func TestPeersAreOptIn(t *testing.T) {
	clearOverrides(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Scope{c.Claude, c.Codex, c.Agy} {
		if len(s.Peers) != 0 {
			t.Errorf("%s: peers %v with HEADROOM_PEERS unset", s.Vendor, s.Peers)
		}
	}

	t.Setenv("HEADROOM_PEERS", "mini2, me@desk.local,mini2  laptop")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	want := []string{"mini2", "me@desk.local", "laptop"}
	if !reflect.DeepEqual(c.Claude.Peers, want) || !reflect.DeepEqual(c.Codex.Peers, want) {
		t.Errorf("peers = %v / %v, want %v for both", c.Claude.Peers, c.Codex.Peers, want)
	}
	if len(c.Agy.Peers) != 0 {
		t.Errorf("agy peers = %v, want none", c.Agy.Peers)
	}
}

// A name reaches ssh as an argument, so one that would read as an option or
// carries what no ssh destination does is never passed on — and never a
// reason for any other command to refuse: it is set apart for check to say,
// and the good names stand.
func TestABadPeerIsSetApartNotRefused(t *testing.T) {
	for _, bad := range []string{"-oProxyCommand=x", "mini2;rm", "a$(b)", "host/x"} {
		clearOverrides(t)
		t.Setenv("HEADROOM_PEERS", "mini2,"+bad)
		c, err := Load()
		if err != nil {
			t.Fatalf("HEADROOM_PEERS with %q refused the configuration: %v", bad, err)
		}
		if !reflect.DeepEqual(c.Claude.Peers, []string{"mini2"}) || !reflect.DeepEqual(c.BadPeers, []string{bad}) {
			t.Errorf("HEADROOM_PEERS with %q: peers %v, set apart %v", bad, c.Claude.Peers, c.BadPeers)
		}
	}
}
