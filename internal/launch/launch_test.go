package launch

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
)

// SecureStorageVar is Claude Code's credential-redirect variable, named here
// so the tests pin the policy table's spelling.
const SecureStorageVar = "CLAUDE_SECURESTORAGE_CONFIG_DIR"

func extra(t *testing.T, dir string) Target {
	t.Helper()
	tgt, err := Extra(config.Claude, dir)
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

func TestEnv(t *testing.T) {
	cases := []struct {
		name   string
		base   []string
		target Target
		want   []string
	}{
		{
			name:   "primary strips inherited var",
			base:   []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/leak", "TERM=xterm"},
			target: Primary(config.Claude),
			want:   []string{"HOME=/u", "TERM=xterm"},
		},
		{
			name:   "primary with clean base changes nothing",
			base:   []string{"HOME=/u", "TERM=xterm"},
			target: Primary(config.Claude),
			want:   []string{"HOME=/u", "TERM=xterm"},
		},
		{
			name:   "extra account gets exactly one entry",
			base:   []string{"HOME=/u"},
			target: extra(t, "/accts/a@x.com"),
			want:   []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/a@x.com"},
		},
		{
			name:   "extra account replaces a different inherited value",
			base:   []string{"CLAUDE_CONFIG_DIR=/leak", "HOME=/u"},
			target: extra(t, "/accts/a@x.com"),
			want:   []string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/a@x.com"},
		},
		{
			name:   "duplicate inherited entries are all removed",
			base:   []string{"CLAUDE_CONFIG_DIR=/one", "HOME=/u", "CLAUDE_CONFIG_DIR=/two"},
			target: Primary(config.Claude),
			want:   []string{"HOME=/u"},
		},
		{
			name:   "prefix-named variables survive",
			base:   []string{"CLAUDE_CONFIG_DIR_BACKUP=/keep", "HOME=/u"},
			target: Primary(config.Claude),
			want:   []string{"CLAUDE_CONFIG_DIR_BACKUP=/keep", "HOME=/u"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.target.Env(c.base); !slices.Equal(got, c.want) {
				t.Errorf("Env(%v) = %v, want %v", c.base, got, c.want)
			}
		})
	}
}

func TestEnvDoesNotMutateBase(t *testing.T) {
	base := []string{"CLAUDE_CONFIG_DIR=/leak", "HOME=/u"}
	orig := slices.Clone(base)
	extra(t, "/accts/a@x.com").Env(base)
	if !slices.Equal(base, orig) {
		t.Errorf("Env mutated its input: %v", base)
	}
}

func TestExtraRefusesEmptyDir(t *testing.T) {
	if _, err := Extra(config.Claude, ""); err == nil {
		t.Error("Extra(\"\") must refuse: it would build a primary-shaped environment under an extra-shaped call")
	}
}

func TestAmbient(t *testing.T) {
	if v, present := Ambient(config.Claude, []string{"HOME=/u"}); present || v != "" {
		t.Errorf("Ambient(clean) = (%q, %v), want absent", v, present)
	}
	if v, present := Ambient(config.Claude, []string{"CLAUDE_CONFIG_DIR=/leak"}); !present || v != "/leak" {
		t.Errorf("Ambient = (%q, %v), want (/leak, true)", v, present)
	}
	// Present-but-empty is present: it is unverified vendor territory, not
	// the verified absent state, and hiding the difference is how a
	// diagnostic goes silent over it.
	if v, present := Ambient(config.Claude, []string{"CLAUDE_CONFIG_DIR="}); !present || v != "" {
		t.Errorf("Ambient(present-empty) = (%q, %v), want (\"\", true)", v, present)
	}
}

func TestConflicts(t *testing.T) {
	cases := []struct {
		name   string
		base   []string
		target Target
		want   bool
	}{
		{"absent never conflicts", []string{"HOME=/u"}, Primary(config.Claude), false},
		// The primary is selected only by absence, so any present value —
		// its own real path and the empty string included — conflicts.
		{"primary vs other dir", []string{"CLAUDE_CONFIG_DIR=/leak"}, Primary(config.Claude), true},
		{"primary vs its own real path", []string{"CLAUDE_CONFIG_DIR=/u/.claude"}, Primary(config.Claude), true},
		{"primary vs present-empty", []string{"CLAUDE_CONFIG_DIR="}, Primary(config.Claude), true},
		{"extra vs exactly its dir", []string{"CLAUDE_CONFIG_DIR=/accts/a"}, extra(t, "/accts/a"), false},
		{"extra vs a different dir", []string{"CLAUDE_CONFIG_DIR=/accts/b"}, extra(t, "/accts/a"), true},
		{"extra vs present-empty", []string{"CLAUDE_CONFIG_DIR="}, extra(t, "/accts/a"), true},
		{"extra vs absent", []string{"HOME=/u"}, extra(t, "/accts/a"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, got := c.target.Conflicts(c.base); got != c.want {
				t.Errorf("Conflicts(%v) = %v, want %v", c.base, got, c.want)
			}
		})
	}
}

// A relative dir is the reproduced chimera: claude reads the *default*
// Keychain item (the primary's login) while writing state beside the cwd.
// Both constructors refuse it, so Env can never emit one.
func TestExtraAndForRefuseRelativeDirs(t *testing.T) {
	for _, dir := range []string{"yan@planlab.ai", "./x", "../x"} {
		if _, err := Extra(config.Claude, dir); err == nil {
			t.Errorf("Extra(%q) accepted a relative dir", dir)
		}
		if _, err := For(config.Claude, dir); err == nil {
			t.Errorf("For(%q) accepted a relative dir", dir)
		}
	}
	if tgt, err := For(config.Claude, ""); err != nil || !tgt.IsPrimary() {
		t.Errorf("For(\"\") = (%v, %v), want the primary", tgt, err)
	}
	if tgt, err := For(config.Claude, "/abs/dir"); err != nil || tgt.IsPrimary() {
		t.Errorf("For(\"/abs/dir\") = (%v, %v), want an extra", tgt, err)
	}
}

// The credential-redirect variable (verified 2.1.220: it selects which
// Keychain item answers, independently of the config dir) must never survive
// into a child: an inherited value would pair one account's tokens with
// another's state on any managed launch.
func TestEnvStripsSecureStorageVar(t *testing.T) {
	base := []string{"HOME=/u", SecureStorageVar + "=/somewhere/else", "TERM=xterm"}
	for _, tgt := range []Target{Primary(config.Claude), extra(t, "/abs/dir")} {
		for _, kv := range tgt.Env(base) {
			if strings.HasPrefix(kv, SecureStorageVar+"=") {
				t.Errorf("%v leaked %q", tgt, kv)
			}
		}
	}
	if got := Redirects(config.Claude, base); len(got) != 1 || got[0].Name != SecureStorageVar || got[0].Value != "/somewhere/else" || got[0].Secret {
		t.Errorf("Redirects = %v", got)
	}
}

// The environment policy table, Codex's row: the home variable set for an
// extra and absent for the primary, every variable that could re-route the
// child or substitute its credentials removed, and Claude Code's variables
// left exactly as they were — a target strips only its own vendor's.
func TestCodexEnv(t *testing.T) {
	base := []string{
		"HOME=/u",
		"CODEX_HOME=/leak",
		"CODEX_SQLITE_HOME=/elsewhere",
		"CODEX_API_KEY=sk-secret",
		"CODEX_ACCESS_TOKEN=tok-secret",
		"CLAUDE_CONFIG_DIR=/accts/claude",
		"OPENAI_API_KEY=left-alone",
	}
	ex, err := Extra(config.Codex, "/codex-accts/a@x.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		target Target
		want   []string
	}{
		{"primary by absence", Primary(config.Codex),
			[]string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/claude", "OPENAI_API_KEY=left-alone"}},
		{"extra sets exactly one home", ex,
			[]string{"HOME=/u", "CLAUDE_CONFIG_DIR=/accts/claude", "OPENAI_API_KEY=left-alone", "CODEX_HOME=/codex-accts/a@x.com"}},
	} {
		if got := c.target.Env(base); !slices.Equal(got, c.want) {
			t.Errorf("%s: Env = %v, want %v", c.name, got, c.want)
		}
	}
	// A Claude Code launch leaves Codex's variables alone.
	for _, kv := range Primary(config.Claude).Env(base) {
		if kv == "CLAUDE_CONFIG_DIR=/accts/claude" {
			t.Error("Claude Code target kept its own inherited home variable")
		}
	}
	if got := Primary(config.Claude).Env(base); !slices.Contains(got, "CODEX_API_KEY=sk-secret") || !slices.Contains(got, "CODEX_HOME=/leak") {
		t.Errorf("Claude Code target stripped Codex variables: %v", got)
	}

	// Every redirect is named; a credential's value never is.
	reds := Redirects(config.Codex, base)
	if len(reds) != 3 {
		t.Fatalf("Redirects = %v, want the three non-home variables", reds)
	}
	for _, r := range reds {
		if strings.Contains(r.String(), "secret") {
			t.Errorf("notice leaks a credential value: %q", r.String())
		}
	}
	if s := reds[0].String(); s != "CODEX_SQLITE_HOME=/elsewhere" {
		t.Errorf("a non-secret redirect keeps its value: %q", s)
	}

	if v, conflict := Primary(config.Codex).Conflicts(base); !conflict || v != "/leak" {
		t.Errorf("primary vs inherited CODEX_HOME: (%q, %v)", v, conflict)
	}
	if _, conflict := ex.Conflicts([]string{"CODEX_HOME=/codex-accts/a@x.com"}); conflict {
		t.Error("an extra's own home must not conflict")
	}
	if _, err := Extra(config.Codex, "relative/home"); err == nil {
		t.Error("a relative Codex home must refuse")
	}
}

// ExecPath hands the child the vendor's own name as argv[0] — Codex dispatches
// on it — not a name compiled in for another vendor. Exec replaces the
// process, so the call runs in a copy of this test binary.
func TestExecPathSetsArgv0(t *testing.T) {
	if os.Getenv("HEADROOM_TEST_EXEC_ARGV0") != "" {
		// `ps -o args=` prints the shell's own argv; the trailing command
		// keeps sh from exec'ing ps in its place.
		err := ExecPath("/bin/sh", os.Getenv("HEADROOM_TEST_EXEC_ARGV0"), []string{"-c", "ps -o args= -p $$; true"}, os.Environ())
		t.Fatalf("exec failed: %v", err)
	}
	for _, name := range []string{"codex", "claude"} {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestExecPathSetsArgv0$")
		cmd.Env = append(os.Environ(), "HEADROOM_TEST_EXEC_ARGV0="+name)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, name+" -c") {
			t.Errorf("argv = %q, want argv[0] %q", got, name)
		}
	}
}
