package main

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"
	"github.com/qiushiyan/clikit/clikittest"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/app"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchop"
)

// testApp is the command line main runs, over a test's host and an empty
// stdin: the later binding of host wins.
func testApp(h host) clikit.App {
	a := cli
	a.Options = append(slices.Clone(cli.Options), kong.Bind(h))
	a.Stdin = strings.NewReader("")
	return a
}

// recorder stands in for the exec that replaces the process, and records
// what it was asked to start.
type recorder struct {
	called    bool
	binary    string
	args, env []string
}

func (r *recorder) exec(_, binary string, args, env []string) error {
	r.called, r.binary, r.args, r.env = true, binary, args, env
	return nil
}

// quietHost reads no credential, sees no process and leaves no refresh
// behind; its exec is rec's.
func quietHost(rec *recorder) host {
	return host{
		place: launchop.Deps{
			Creds:   func(accounts.Account) string { return "" },
			Probe:   func(int) (int64, error) { return 0, os.ErrNotExist },
			Refresh: func(config.Scope) error { return nil },
		},
		round: app.LiveRound,
		exec:  rec.exec,
	}
}

// isolate gives the run a home of its own and a PATH holding no vendor, and
// returns the home. No usage URL is live should anything try one.
func isolate(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEADROOM_HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HEADROOM_USAGE_URL", "http://127.0.0.1:1/usage")
	t.Setenv("HEADROOM_CODEX_USAGE_URL", "http://127.0.0.1:1/usage")
	return home
}

// seed makes Claude Code's primary and the named extras in home, none logged in.
func seed(t *testing.T, home string, extras ...string) {
	t.Helper()
	scope := config.ForHome(home).Claude
	if err := os.MkdirAll(filepath.Join(scope.PrimaryDir(), "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, email := range extras {
		if _, _, _, err := accounts.Seed(scope, email, accounts.SeedOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// tree is every entry under root with its content, a symlink's target or
// "dir", so two readings compare a run's effects.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			out[rel] = "-> " + target
			return err
		case d.IsDir():
			out[rel] = "dir"
		default:
			data, err := os.ReadFile(p)
			out[rel] = string(data)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// What the grammar refuses, it refuses before anything runs: exit 2, a line
// naming what was wrong, nothing on stdout, no vendor started and no file in
// the home written. The app package's tests call surfaces with typed options
// and cannot reach these.
func TestGrammarRefuses(t *testing.T) {
	home := isolate(t)
	seed(t, home, "a@x.com", "b@x.com")
	before := tree(t, home)
	rec := &recorder{}
	a := testApp(quietHost(rec))

	for _, c := range []struct {
		args []string
		want string
	}{
		// A launch decides one way, and records or previews, not both.
		{[]string{"launch", "--auto", "--last"}, "--auto and --last can't be used together"},
		{[]string{"launch", "--auto", "--account", "a@x.com"}, "can't be used together"},
		{[]string{"launch", "--last", "--account", "a@x.com"}, "can't be used together"},
		{[]string{"launch", "--last", "--remember"}, "can't be used together"},
		{[]string{"launch", "--dry-run", "--remember"}, "can't be used together"},
		{[]string{"launch", "--resume", "abc"}, "unknown flag --resume"},
		{[]string{"launch", "--vendor", "gemini"}, "--vendor"},

		// An empty name is a caller's expansion bug, never "the pinned account".
		{[]string{"launch", "--account", ""}, "an account name cannot be empty"},
		{[]string{"resolve", ""}, "an account name cannot be empty"},
		{[]string{"limits", "--account", ""}, "an account name cannot be empty"},
		{[]string{"login", ""}, "an account name cannot be empty"},
		{[]string{"headless", "place", "--exclude", ""}, "an account name cannot be empty"},
		{[]string{"headless", "token", ""}, "an account name cannot be empty"},
		{[]string{"headless", "place", "--exclude"}, "--exclude"},
		{[]string{"limits", "--vendor"}, "--vendor"},

		{[]string{"headless", "started", "", "5"}, "a placement id cannot be empty"},
		{[]string{"headless", "started", "id", "0"}, "0 is not a process id"},
		{[]string{"headless", "started", "id", "many"}, "pid"},

		{[]string{"launches", "-n"}, "count"},
		{[]string{"launches", "-n", "many"}, "count"},
		{[]string{"launches", "--count=-1"}, "-n -1 is not a count"},
		{[]string{"launches", "--nope"}, "unknown flag --nope"},
		{[]string{"launches", "--eval", "-n", "3"}, "-n does not apply"},
		{[]string{"launches", "--eval", "-n", "20"}, "-n does not apply"}, // given, though it is the default
		{[]string{"launches", "--since", "7d"}, "--since narrows --eval"},
		{[]string{"launches", "--eval", "--since"}, "--since"},
		{[]string{"launches", "--eval", "--since", "soon"}, "soon"},

		{[]string{"login", "--within", "x"}, "--within"},
		{[]string{"login", "--within", "-1"}, "--within"},
		{[]string{"login", "--within=-1"}, "--within -1 is not a number of days"},
		{[]string{"login", "--within=106752"}, "is not a number of days"}, // past what a duration counts
		{[]string{"login", "--bogus"}, "unknown flag --bogus"},
		{[]string{"login", "--all", "a@x.com"}, "--all and account names exclude each other"},

		{[]string{"accounts", "add"}, "<email>"},
		{[]string{"accounts", "add", "c@x.com", "extra"}, "unexpected argument extra"},
		{[]string{"accounts", "add", "--share-config=", "c@x.com"}, "--share-config= needs a directory"},
		{[]string{"accounts", "bogus"}, "unexpected argument bogus"},
		{[]string{"accounts", "remove", ""}, "name the account to remove"},
		{[]string{"accounts", "ledger", "--vendor", "codex"}, "only Claude Code homes share a ledger"},

		// A default command's flags never reach a command chosen over it.
		{[]string{"accounts", "--compact", "remove", "--yes", "a@x.com"}, "--compact belongs to accounts board, which accounts remove does not run"},
		{[]string{"accounts", "--json", "ledger"}, "--json belongs to accounts board"},
		{[]string{"--json", "sessions"}, "--json belongs to board, which sessions does not run"},
		{[]string{"--compact", "launch", "--account", "a@x.com"}, "--compact belongs to board"},
		{[]string{"--vendor", "codex", "resolve"}, "--vendor belongs to board"},
		{[]string{"headless", "--json", "token", "a@x.com"}, "--json belongs to headless list"},

		{[]string{"sessions", "--json", "--cd-file", "/x"}, "--json takes no other arguments"},
		{[]string{"sessions", "--json", "--", "--fast"}, "--json takes no other arguments"},

		// Retired spellings, and words that are no command.
		{[]string{"select"}, "unexpected argument select"},
		{[]string{"select", "add"}, "unexpected argument select"},
		{[]string{"--check"}, "unknown flag --check"},
		{[]string{"lauch"}, "unexpected argument lauch"},
		{[]string{"check", "extra"}, "unexpected argument extra"},
		{[]string{"version", "extra"}, "unexpected argument extra"},
	} {
		r := clikittest.Run(a, c.args...)
		if r.Code != 2 || r.Stdout != "" || !strings.Contains(r.Stderr, c.want) {
			t.Errorf("%q: exit %d, want 2 saying %q\nstdout: %s\nstderr: %s", c.args, r.Code, c.want, r.Stdout, r.Stderr)
		}
	}
	if rec.called {
		t.Errorf("a refused command started %s %v", rec.binary, rec.args)
	}
	if after := tree(t, home); !maps.Equal(before, after) {
		t.Errorf("a refused command changed the home:\nbefore %v\nafter  %v", before, after)
	}
}

// parse decodes argv into the grammar without running anything.
func parse(t *testing.T, args ...string) (*CLI, *kong.Context) {
	t.Helper()
	g := &CLI{}
	k, err := kong.New(g, cli.Options...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := k.Parse(args)
	if err != nil {
		t.Fatalf("%q: %v", args, err)
	}
	return g, ctx
}

// What reaches the vendor is what the caller typed after headroom's own
// flags, with the one `--` that ended them removed; a name with a comma in it
// is one name; --vendor is the accounts command's wherever it is typed.
func TestGrammarDecodes(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"launch", "fix", "it"}, []string{"fix", "it"}},
		{[]string{"launch", "--"}, []string{}},
		{[]string{"launch", "--", "--resume", "x"}, []string{"--resume", "x"}},
		{[]string{"launch", "--account", "a@x.com", "--", "--", "--fast"}, []string{"--", "--fast"}},
		{[]string{"launch", "--vendor", "codex", "--account", "c@x.com", "--", "exec", "--vendor", "x"}, []string{"exec", "--vendor", "x"}},
	} {
		g, _ := parse(t, c.args...)
		if got := vendorArgs(g.Launch.Args); !slices.Equal(got, c.want) && len(got)+len(c.want) > 0 {
			t.Errorf("%q: vendor args %q, want %q", c.args, got, c.want)
		}
	}
	if g, _ := parse(t, "launch", "--vendor", "codex", "--account", "c@x.com", "--", "exec", "--vendor", "x"); g.Launch.Vendor != config.Codex || g.Launch.Account != "c@x.com" {
		t.Errorf("launch --vendor codex: vendor %q account %q", g.Launch.Vendor, g.Launch.Account)
	}
	if g, _ := parse(t, "sessions", "--", "--dangerously-skip-permissions"); !slices.Equal(vendorArgs(g.Sessions.Args), []string{"--dangerously-skip-permissions"}) {
		t.Errorf("sessions args %q", g.Sessions.Args)
	}

	if g, _ := parse(t, "headless", "place", "--exclude", "a@x.com,b@x.com", "--exclude", "c@x.com"); !slices.Equal(names(g.Headless.Place.Exclude), []string{"a@x.com,b@x.com", "c@x.com"}) {
		t.Errorf("--exclude %q", g.Headless.Place.Exclude)
	}
	if g, _ := parse(t, "login", "a@x.com,b@x.com"); !slices.Equal(names(g.Login.Accounts), []string{"a@x.com,b@x.com"}) {
		t.Errorf("login names %q", g.Login.Accounts)
	}

	for _, args := range [][]string{
		{"accounts", "add", "--share-config", "c@x.com"},
		{"accounts", "add", "c@x.com", "--share-config"},
	} {
		if g, _ := parse(t, args...); g.Accounts.Add.Email != "c@x.com" || !g.Accounts.Add.ShareConfig.set || g.Accounts.Add.ShareConfig.dir != "" {
			t.Errorf("%q: email %q share %+v", args, g.Accounts.Add.Email, g.Accounts.Add.ShareConfig)
		}
	}
	if g, _ := parse(t, "accounts", "add", "--share-config=/d", "c@x.com"); g.Accounts.Add.ShareConfig.dir != "/d" {
		t.Errorf("--share-config=/d: %+v", g.Accounts.Add.ShareConfig)
	}

	for _, args := range [][]string{
		{"accounts", "--vendor", "codex", "add", "c@x.com"},
		{"accounts", "add", "--vendor", "codex", "c@x.com"},
		{"accounts", "add", "c@x.com", "--vendor", "codex"},
		{"accounts", "remove", "--vendor", "codex", "c@x.com"},
	} {
		if g, _ := parse(t, args...); g.Accounts.Vendor == nil || *g.Accounts.Vendor != config.Codex {
			t.Errorf("%q: vendor %v", args, g.Accounts.Vendor)
		}
	}
	if g, _ := parse(t, "accounts", "add", "c@x.com"); g.Accounts.Vendor != nil {
		t.Errorf("accounts add with no --vendor: %v", *g.Accounts.Vendor)
	}

	// -n is given or not, whatever its value: --eval refuses it typed as 20.
	if _, ctx := parse(t, "launches", "--eval"); given(ctx, "count") {
		t.Error("launches --eval: -n reads as given")
	}
	if _, ctx := parse(t, "launches", "-n", "20"); !given(ctx, "count") {
		t.Error("launches -n 20: -n reads as absent")
	}
}

// Help and the retired verb's tombstone speak whatever the configuration
// says: the shell the tombstone diagnoses is stale, and stale shells come
// with old environments headroom may now refuse. Every other command refuses
// that configuration with exit 1, and check says it as the FAIL it is.
func TestBrokenConfiguration(t *testing.T) {
	isolate(t)
	t.Setenv("HEADROOM_HOME", "relative")
	a := testApp(quietHost(&recorder{}))

	for _, args := range [][]string{{"--help"}, {"launch", "--help"}, {"help", "accounts", "add"}, {"--version"}, {"version"}} {
		if r := clikittest.Run(a, args...); r.Code != 0 || r.Stdout == "" || r.Stderr != "" {
			t.Errorf("%q: exit %d\nstdout: %s\nstderr: %s", args, r.Code, r.Stdout, r.Stderr)
		}
	}
	for _, args := range [][]string{{"resume"}, {"resume", "--json"}, {"resume", "--help"}, {"resume", "--version"}} {
		r := clikittest.Run(a, args...)
		if r.Code != 2 || r.Stdout != "" || !strings.Contains(r.Stderr, "`resume` was retired") || !strings.Contains(r.Stderr, "exec zsh") {
			t.Errorf("%q: exit %d\nstdout: %s\nstderr: %s", args, r.Code, r.Stdout, r.Stderr)
		}
	}
	for _, args := range [][]string{{"limits"}, {"launch", "--account", "a@x.com"}, {}} {
		if r := clikittest.Run(a, args...); r.Code != 1 || !strings.Contains(r.Stderr, "HEADROOM_HOME") {
			t.Errorf("%q: exit %d, want 1\nstderr: %s", args, r.Code, r.Stderr)
		}
	}
	r := clikittest.Run(a, "check")
	if r.Code != 1 || !strings.HasPrefix(r.Stdout, "FAIL  config:") || !strings.Contains(r.Stderr, "HEADROOM_HOME") {
		t.Errorf("check: exit %d\nstdout: %s\nstderr: %s", r.Code, r.Stdout, r.Stderr)
	}
}

// A machine without Codex: every command naming it fails with that reason —
// except accounts add, which is how the first Codex directory comes to exist.
// Then every surface reaches Codex through --vendor, and a command without it
// means what it meant: Claude Code.
func TestCommandsOnAnAbsentVendor(t *testing.T) {
	home := isolate(t)
	rec := &recorder{}
	a := testApp(quietHost(rec))
	run := func(args ...string) clikittest.Result {
		t.Helper()
		return clikittest.Run(a, args...)
	}

	for _, args := range [][]string{
		{"launch", "--vendor", "codex", "--account", "a@x.com"},
		{"resolve", "--vendor", "codex"},
		{"limits", "--vendor", "codex"},
		{"accounts", "remove", "--vendor", "codex", "a@x.com"},
		{"accounts", "--vendor", "codex"},
		{"--json", "--vendor", "codex"},
		{"refresh", "--vendor", "codex"},
	} {
		if r := run(args...); r.Code != 1 || !strings.Contains(r.Stderr, "Codex was not found on this machine") {
			t.Errorf("%q: exit %d, want 1\nstderr: %s", args, r.Code, r.Stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex-accounts")); err == nil {
		t.Fatal("a refused command created the Codex accounts root")
	}

	r := run("accounts", "add", "--vendor", "codex", "new@x.com")
	if r.Code != 0 || !strings.Contains(r.Stdout, "seeded "+filepath.Join(home, ".codex-accounts", "new@x.com")) {
		t.Fatalf("accounts add --vendor codex: exit %d\nstdout: %s\nstderr: %s", r.Code, r.Stdout, r.Stderr)
	}
	if target, err := os.Readlink(filepath.Join(home, ".codex-accounts", "new@x.com", "sessions")); err != nil || target != filepath.Join(home, ".codex", "sessions") {
		t.Errorf("sessions link = %q, %v", target, err)
	}
	if r := run("resolve", "--vendor", "codex", "new@x.com"); r.Code != 0 || r.Stdout != "new@x.com\t"+filepath.Join(home, ".codex-accounts", "new@x.com")+"\textra\n" {
		t.Errorf("resolve after add: exit %d %q %s", r.Code, r.Stdout, r.Stderr)
	}
	// --vendor before remove reaches it: Codex accounts are never removed.
	if r := run("accounts", "--vendor", "codex", "remove", "--yes", "new@x.com"); r.Code != 1 || !strings.Contains(r.Stderr, "never removes one") {
		t.Errorf("accounts --vendor codex remove: exit %d\nstderr: %s", r.Code, r.Stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex-accounts", "new@x.com")); err != nil {
		t.Errorf("the Codex account is gone: %v", err)
	}

	// The reporting surfaces: unfiltered they hold both vendors, and --vendor
	// restricts accounts and current to one.
	type document struct {
		Current  map[string]string `json:"current"`
		Accounts []struct {
			Vendor string `json:"vendor"`
		} `json:"accounts"`
	}
	decode := func(args ...string) document {
		t.Helper()
		r := run(args...)
		var d document
		if err := json.Unmarshal([]byte(r.Stdout), &d); err != nil {
			t.Fatalf("%q: exit %d, %v\nstdout: %s\nstderr: %s", args, r.Code, err, r.Stdout, r.Stderr)
		}
		return d
	}
	all, only, fetched := decode("limits"), decode("limits", "--vendor", "codex"), decode("--json", "--vendor", "codex")
	if len(all.Current) != 2 || len(only.Current) != 1 || len(only.Accounts) != 2 {
		t.Errorf("limits: unfiltered current %v; filtered current %v, %d accounts", all.Current, only.Current, len(only.Accounts))
	}
	for _, d := range []document{only, fetched} {
		if len(d.Current) != 1 || len(d.Accounts) != 2 {
			t.Errorf("--vendor codex: current %v, %d accounts", d.Current, len(d.Accounts))
		}
		for _, acct := range d.Accounts {
			if acct.Vendor != "codex" {
				t.Errorf("--vendor codex returned a %s account", acct.Vendor)
			}
		}
	}

	// A launch: --vendor reaches the Codex scope, the binary is Codex's, and
	// the child's own arguments pass through untouched.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if r := run("launch", "--vendor", "codex", "--account", "new@x.com", "--", "exec", "--vendor", "x"); r.Code != 0 {
		t.Errorf("launch --vendor codex: exit %d\nstderr: %s", r.Code, r.Stderr)
	}
	if !rec.called || rec.binary != "codex" || strings.Join(rec.args, " ") != "exec --vendor x" {
		t.Errorf("launch: called=%v argv0=%q args=%q", rec.called, rec.binary, rec.args)
	}
	if !slices.Contains(rec.env, "CODEX_HOME="+filepath.Join(home, ".codex-accounts", "new@x.com")) {
		t.Errorf("CODEX_HOME not the account's: %q", rec.env)
	}

	if r := run("resolve"); r.Code != 0 || !strings.HasSuffix(r.Stdout, filepath.Join(home, ".claude")+"\tprimary\n") {
		t.Errorf("bare resolve: exit %d %q %s", r.Code, r.Stdout, r.Stderr)
	}
}

// Every help page, as an agent reads it; `make golden` accepts a change.
func TestHelpPages(t *testing.T) {
	isolate(t)
	clikittest.Golden(t, testApp(quietHost(&recorder{})), "testdata/help")
}
