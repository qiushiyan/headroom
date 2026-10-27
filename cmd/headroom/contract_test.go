package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/state"
)

// The invocations other repositories run, byte for byte: what each prints on
// both streams, its exit status, and for a launch what the vendor was started
// with. The consumers are named in AGENTS.md § Contracts; the shapes come from
// their source (dotfiles' zsh launchers and quota mod, planlab's steward,
// skill-evals, brief, envoy, gazette) and from headroom's own detached
// refresh. A file under testdata/contract that changes changes what one of
// them reads, so change it in step; HEADROOM_CONTRACT_UPDATE=1 rewrites them.
//
// The binary is built and run as a process, in a home of its own with stub
// vendors and no terminal on any stream, so the pins hold across any change
// inside it.
func TestContracts(t *testing.T) {
	w := newWorld(t)
	share := filepath.Join(w.base, "shared-config")
	if err := os.MkdirAll(filepath.Join(share, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	cdFile := filepath.Join(w.base, "cwd")
	for _, c := range []struct {
		name  string
		args  []string
		stdin string
	}{
		// Read before anything is launched: the board, the documents, the line
		// shell preflight splits on tabs, and the version a host install pins.
		{"version", []string{"version"}, ""},
		{"version-flag", []string{"--version"}, ""},
		{"limits", []string{"limits"}, ""},
		{"limits-claude", []string{"limits", "--vendor", "claude"}, ""},
		{"limits-account", []string{"limits", "--account", "a@x.com"}, ""},
		{"board-json", []string{"--json"}, ""},
		{"board", nil, ""},
		{"accounts", []string{"accounts"}, ""},
		{"accounts-compact", []string{"accounts", "--compact"}, ""},
		{"accounts-compact-codex", []string{"accounts", "--compact", "--vendor", "codex"}, ""},
		{"resolve", []string{"resolve", "a@x.com"}, ""},
		{"resolve-current", []string{"resolve"}, ""},
		{"resolve-codex", []string{"resolve", "--vendor", "codex", "c@x.com"}, ""},
		{"sessions-json", []string{"sessions", "--json"}, ""},
		{"headless-list", []string{"headless"}, ""},
		{"headless-json", []string{"headless", "--json"}, ""},
		{"login-dry-run", []string{"login", "--dry-run"}, ""},
		{"launch-dry-run", []string{"launch", "--dry-run"}, ""},
		{"check", []string{"check"}, ""},
		{"refresh", []string{"refresh", "--vendor", "claude"}, ""},

		// Launches, in the shapes the launchers and envoy prefixes send.
		{"launch-named", []string{"launch", "--account", "a@x.com", "--"}, ""},
		{"launch-named-dashdash", []string{"launch", "--account", "a@x.com", "--", "--", "--fast"}, ""},
		{"launch-bare", []string{"launch", "--", "--dangerously-skip-permissions", "--settings", `{"fastMode":true}`}, ""},
		{"launch-auto", []string{"launch", "--auto", "--", "-p", "--output-format", "stream-json", "--verbose",
			"--resume", "12345678-1234-4123-8123-123456789abc"}, ""},
		{"launch-codex-named", []string{"launch", "--vendor", "codex", "--account", "c@x.com", "--", "resume", "--all"}, ""},
		{"launch-codex-bare", []string{"launch", "--vendor", "codex", "--"}, ""},
		{"launch-codex-exec", []string{"launch", "--vendor", "codex", "--", "exec", "--json", "hello"}, ""},
		{"launches", []string{"launches"}, ""},
		{"launches-json", []string{"launches", "--json"}, ""},
		{"launches-n", []string{"launches", "-n", "2"}, ""},
		{"launches-eval-json", []string{"launches", "--eval", "--json"}, ""},

		// The session picker needs a terminal; the cd file says nothing was entered.
		{"sessions-no-terminal", []string{"sessions", "--cd-file", cdFile, "--", "--dangerously-skip-permissions"}, ""},

		// Headless placement, as the eval runner drives it.
		{"headless-place", []string{"headless", "place", "--prefer", "a@x.com", "--exclude", "b@x.com"}, ""},
		{"headless-place-refused", []string{"headless", "place", "--exclude", "a@x.com", "--exclude", "b@x.com"}, ""},
		{"headless-token", []string{"headless", "token", "a@x.com"}, ""},
		{"headless-forget", []string{"headless", "forget", "b@x.com"}, ""},

		// The account set, in the three spellings of --share-config in use.
		{"accounts-add-share-dir", []string{"accounts", "add", "--share-config=" + share, "d@x.com"}, ""},
		{"accounts-add-codex-share", []string{"accounts", "add", "--vendor", "codex", "--share-config", "e@x.com"}, ""},
		{"accounts-add-share-after", []string{"accounts", "add", "f@x.com", "--share-config"}, ""},
		{"accounts-remove-yes", []string{"accounts", "remove", "--yes", "f@x.com"}, ""},
		{"accounts-remove-no-terminal", []string{"accounts", "remove"}, ""},
		{"accounts-ledger", []string{"accounts", "ledger"}, ""},
		{"accounts-ledger-own", []string{"accounts", "ledger", w.claude.AccountsRoot}, ""},

		// Refusals a caller can meet.
		{"resume-tombstone", []string{"resume", "--json"}, ""},
		{"resume-help", []string{"resume", "--help"}, ""},
		{"resume-version", []string{"resume", "--version"}, ""},
		{"launch-empty-account", []string{"launch", "--account", "", "--"}, ""},
		{"launch-unknown-flag", []string{"launch", "--resume", "abc"}, ""},
		{"launch-auto-last", []string{"launch", "--auto", "--last", "--"}, ""},
		{"check-argument", []string{"check", "extra"}, ""},
		{"unknown-command", []string{"lauch"}, ""},
		{"accounts-board-flag-before-remove", []string{"accounts", "--compact", "remove", "--yes", "a@x.com"}, ""},
		{"json-before-command", []string{"--json", "sessions"}, ""},
		{"launch-words-without-dashdash", []string{"launch", "fix", "it"}, ""},
		{"launches-eval-n-default", []string{"launches", "--eval", "-n", "20"}, ""},
		{"launches-eval-n-zero", []string{"launches", "--eval", "-n", "0"}, ""},
		{"launches-since-without-eval", []string{"launches", "--since", "7d"}, ""},
		{"login-within-negative", []string{"login", "--dry-run", "--within", "-1"}, ""},
		{"headless-place-comma", []string{"headless", "place", "--dry-run", "--exclude", "a@x.com,b@x.com"}, ""},
		{"headless-place-empty", []string{"headless", "place", "--exclude", ""}, ""},
		{"accounts-remove-empty", []string{"accounts", "remove", ""}, ""},
		{"accounts-vendor-before-add", []string{"accounts", "--vendor", "codex", "add", "g@x.com"}, ""},
		{"accounts-ledger-codex", []string{"accounts", "ledger", "--vendor", "codex"}, ""},

		// The plan as an agent reads it, and a handed-off sign-in end to end,
		// last: the renewal changes a@x.com's login. (docs/specs/login-handoff.md)
		{"login-dry-run-json", []string{"login", "--dry-run", "--json"}, ""},
		{"login-json-without-dry-run", []string{"login", "--json"}, ""},
		{"login-status-none", []string{"login", "status", "a@x.com"}, ""},
		{"login-finish-none", []string{"login", "finish", "a@x.com"}, "code#state\n"},
		{"login-finish-empty", []string{"login", "finish", "a@x.com"}, ""},
		{"login-start", []string{"login", "start", "a@x.com"}, ""},
		{"login-start-again", []string{"login", "start", "a@x.com"}, ""},
		{"login-finish-other", []string{"login", "finish", "a@x.com"}, "code#another-state\n"},
		{"login-finish", []string{"login", "finish", "a@x.com"}, "good#contract-state\n"},
		{"login-status", []string{"login", "status", "a@x.com"}, ""},
		{"login-cancel-ended", []string{"login", "cancel", "a@x.com"}, ""},
	} {
		got := w.runIn(t, c.stdin, c.args...)
		pinFile(t, c.name, got)
		if c.name == "headless-place" {
			// The runner reports the session it started under the placement's
			// id, read from the document place printed.
			id := placementID.FindStringSubmatch(w.lastStdout)
			if id == nil {
				t.Fatalf("headless place printed no placement id:\n%s", w.lastStdout)
			}
			pinFile(t, "headless-started", w.run(t, "headless", "started", id[1], fmt.Sprint(os.Getpid())))
		}
	}
}

var placementID = regexp.MustCompile(`"placement": "([^"]+)"`)

// world is one home holding three Claude Code accounts and two Codex ones,
// each logged in with an access token that has aged out — so no surface asks
// the network and none leaves a refresh running — and each with usage on disk
// observed ten minutes ago.
type world struct {
	base, home, stubs, record string
	claude, codex             config.Scope
	bin                       string
	lastStdout                string // the newest run's stdout, unnormalized
}

func newWorld(t *testing.T) *world {
	t.Helper()
	// Short enough for a handed-off sign-in's socket, which t.TempDir is not.
	short, err := os.MkdirTemp("/tmp", "hrc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	base, err := filepath.EvalSymlinks(short)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{base: base, home: filepath.Join(base, "home"), stubs: filepath.Join(base, "stubs"), record: filepath.Join(base, "record")}
	cfg := config.ForHome(w.home)
	w.claude, w.codex = cfg.Claude, cfg.Codex
	// HOME is not this user's login home, so the binary spells the primary
	// out and reads its identity inside its dir, as a second home does.
	w.claude.PrimaryExplicit = true
	w.codex.Present = true

	aged := time.Now().Add(-time.Hour).UnixMilli()
	far := time.Now().Add(25 * 24 * time.Hour).UnixMilli()
	claudeLogin := func(dir, email string) {
		write(t, filepath.Join(dir, ".claude.json"), fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"uuid-%s"}}`, email, email))
		write(t, filepath.Join(dir, ".credentials.json"),
			fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-%s","expiresAt":%d,"refreshTokenExpiresAt":%d,"subscriptionType":"max"}}`, email, aged, far))
	}
	if err := os.MkdirAll(filepath.Join(w.claude.PrimaryDir(), "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeLogin(w.claude.PrimaryDir(), "p@x.com")
	for _, email := range []string{"a@x.com", "b@x.com"} {
		dir, _, _, err := accounts.Seed(w.claude, email, accounts.SeedOptions{})
		if err != nil {
			t.Fatal(err)
		}
		claudeLogin(dir, email)
	}

	codexLogin := func(dir string, n int, email string) {
		codexauthtest.Login{Email: email, Plan: "plus", AccountID: fmt.Sprintf("acct-%d", n), UserID: fmt.Sprintf("user-%d", n),
			Token: fmt.Sprintf("tok-%d", n), Expires: time.Now().Add(-time.Hour)}.Write(t, dir)
	}
	codexLogin(w.codex.PrimaryDir(), 1, "q@x.com")
	dir, _, _, err := accounts.Seed(w.codex, "c@x.com", accounts.SeedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	codexLogin(dir, 2, "c@x.com")

	reset := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	observe(t, w.claude, map[string]string{
		"p":       fmt.Sprintf(`{"limits":[{"kind":"session","group":"session","percent":12,"resets_at":%q},{"kind":"weekly_all","group":"weekly","percent":40,"resets_at":%q}]}`, reset(150*time.Minute), reset(100*time.Hour)),
		"a@x.com": fmt.Sprintf(`{"limits":[{"kind":"session","group":"session","percent":3,"resets_at":%q},{"kind":"weekly_all","group":"weekly","percent":22,"resets_at":%q}]}`, reset(200*time.Minute), reset(50*time.Hour)),
		"b@x.com": fmt.Sprintf(`{"limits":[{"kind":"session","group":"session","percent":85,"resets_at":%q},{"kind":"weekly_all","group":"weekly","percent":61,"resets_at":%q}]}`, reset(30*time.Minute), reset(130*time.Hour)),
	})
	codexUsage := func(n, percent int) string {
		return fmt.Sprintf(`{"plan_type":"plus","account_id":"acct-%d","user_id":"user-%d","rate_limit":{"allowed":true,"limit_reached":false,`+
			`"primary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_after_seconds":1000,"reset_at":%d},"secondary_window":null},`+
			`"spend_control":{"reached":false},"rate_limit_reached_type":null}`, n, n, percent, time.Now().Add(72*time.Hour).Unix())
	}
	observe(t, w.codex, map[string]string{"q": codexUsage(1, 30), "c@x.com": codexUsage(2, 70)})

	// Headless tokens for two accounts, minted for the subscription each dir
	// is logged in to.
	keys := keysOf(t, w.claude)
	for _, name := range []string{"a@x.com", "b@x.com"} {
		tok := headless.Token{Value: "sk-ant-oat01-" + strings.Repeat("A", 40) + strings.NewReplacer("@", "_", ".", "_").Replace(name),
			MintedMS: time.Now().Add(-time.Hour).UnixMilli(), EndsMS: time.Now().Add(300 * 24 * time.Hour).UnixMilli(), Key: keys[name].ID()}
		if err := headless.Put(w.claude.AccountsRoot, name, tok); err != nil {
			t.Fatal(err)
		}
	}

	// The vendors record what they were started with; the Keychain holds nothing.
	stubs := map[string]string{
		"claude": `#!/bin/sh
case "$1 $2" in
"auth status") echo '{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}'; exit 0 ;;
"auth login")
  { echo "claude $*"; echo "CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR-<unset>}"; echo ---; } >> "$RECORD"
  echo "Opening browser to sign in…"
  echo "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=contract-state"
  "$BROWSER" "https://claude.com/cai/oauth/authorize?code=true&redirect_uri=http%3A%2F%2Flocalhost%3A4000%2Fcallback&state=contract-state"
  printf 'Paste code here if prompted > '
  read -r code || exec sleep 600
  echo "code=$code" >> "$RECORD"
  [ "$code" = "good#contract-state" ] || { echo "Login failed: That code wasn't accepted."; exit 1; }
  printf '{"claudeAiOauth":{"accessToken":"new","expiresAt":1,"refreshTokenExpiresAt":%s000,"subscriptionType":"max"}}' "$(( $(date +%s) + 30*86400 ))" > "$CLAUDE_CONFIG_DIR/.credentials.json"
  echo "Login successful."; exit 0 ;;
esac
{ echo "claude $*"; echo "CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR-<unset>}"; echo ---; } >> "$RECORD"
`,
		"codex": `#!/bin/sh
case "$1 $2" in
"login status") echo "Logged in using ChatGPT"; exit 0 ;;
esac
{ echo "codex $*"; echo "CODEX_HOME=${CODEX_HOME-<unset>}"; echo ---; } >> "$RECORD"
`,
		"security": "#!/bin/sh\nexit 44\n",
		"open":     "#!/bin/sh\necho \"open $*\" >> \"$RECORD\"\n",
	}
	if err := os.MkdirAll(w.stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(w.stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.bin = headroomBinary(t)
	return w
}

// observe stores one usage response per account through the store's own
// claim and completion, as taken ten minutes ago.
func observe(t *testing.T, scope config.Scope, bodies map[string]string) {
	t.Helper()
	st := state.Open(scope)
	at := time.Now().Add(-10 * time.Minute)
	for name, key := range keysOf(t, scope) {
		body, ok := bodies[name]
		if !ok {
			continue
		}
		dec, err := st.Claim([]state.Key{key}, at)
		if err != nil || !dec[0].Permit {
			t.Fatalf("claim %s: %+v %v", name, dec, err)
		}
		if _, err := st.Complete(key, dec[0].Generation, state.OutcomeStored, []byte(body), at); err != nil {
			t.Fatal(err)
		}
	}
}

func keysOf(t *testing.T, scope config.Scope) map[string]state.Key {
	t.Helper()
	keys := map[string]state.Key{}
	for _, a := range accounts.Discover(scope).Accounts {
		keys[a.Name] = state.Key{UUID: a.AccountID, Name: a.Name}
	}
	return keys
}

// run executes the binary as a caller would, without a terminal, and renders
// what came back: the exit status, both streams and anything a vendor stub
// recorded, with the paths, instants and ids that differ between runs
// replaced by names.
func (w *world) run(t *testing.T, args ...string) string {
	t.Helper()
	return w.runIn(t, "", args...)
}

// runIn is run with stdin on the caller's text.
func (w *world) runIn(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	os.Remove(w.record)
	cmd := exec.Command(w.bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Dir = w.home
	cmd.Env = []string{
		"HOME=" + w.home, "USER=" + os.Getenv("USER"),
		"PATH=" + w.stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"RECORD=" + w.record, "TZ=UTC",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	w.lastStdout = stdout.String()
	var b strings.Builder
	fmt.Fprintf(&b, "$ headroom %s\nexit %d\n--- stdout\n%s--- stderr\n%s", quoteArgs(args), code, stdout.String(), stderr.String())
	if rec, err := os.ReadFile(w.record); err == nil {
		fmt.Fprintf(&b, "--- vendor\n%s", rec)
	}
	return w.normalize(b.String())
}

// What differs between two runs of the same case, each replaced by a name:
// the temporary directory, the build's commit, instants (RFC 3339, epoch
// seconds and milliseconds, the clock and day a board or log line shows them
// at, the date a login or token end is named by), spans counted in seconds
// from now, process ids, the Keychain item a dir's path names and the random
// id a placement is given. Spans a person
// reads ("in 3.3h", "10m ago") are kept: the fixture fixes them relative to
// now, and their rounding absorbs the seconds a run takes.
var (
	version     = regexp.MustCompile(`(?m)^headroom [0-9a-f]{40}(\+dirty)?( \S+)?$`)
	rfc3339     = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)
	epochMS     = regexp.MustCompile(`"(at_ms|[a-z_]+_ms)":\s*\d+`)
	weekdayTime = regexp.MustCompile(`\b(Mon|Tue|Wed|Thu|Fri|Sat|Sun) \d\d:\d\d\b`)
	resetTime   = regexp.MustCompile(`resets \d\d:\d\d\b`)
	logWhen     = regexp.MustCompile(`(?m)^\d\d-\d\d \d\d:\d\d\b`)
	monthDay    = regexp.MustCompile(`\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{1,2} \(`)
	pid         = regexp.MustCompile(`"pid":\s*\d+`)
	seconds     = regexp.MustCompile(`"([a-z0-9_]+_s)":\s*\d+`)
	keychain    = regexp.MustCompile(`credentials-[0-9a-f]{8}\b`)
	placement   = regexp.MustCompile(`"placement": "[0-9a-f]+"`)
	startedArgs = regexp.MustCompile(`headless started [0-9a-f]+ \d+`)
)

func (w *world) normalize(s string) string {
	s = strings.ReplaceAll(s, w.base, "$BASE")
	s = version.ReplaceAllString(s, "headroom <rev> <time>")
	s = rfc3339.ReplaceAllString(s, "<time>")
	s = epochMS.ReplaceAllString(s, `"$1": <ms>`)
	s = weekdayTime.ReplaceAllString(s, "<day> <clock>")
	s = resetTime.ReplaceAllString(s, "resets <clock>")
	s = logWhen.ReplaceAllString(s, "<mm-dd> <clock>")
	s = monthDay.ReplaceAllString(s, "<date> (")
	s = pid.ReplaceAllString(s, `"pid": <pid>`)
	s = seconds.ReplaceAllString(s, `"$1": <s>`)
	s = keychain.ReplaceAllString(s, "credentials-<dir-hash>")
	s = placement.ReplaceAllString(s, `"placement": "<id>"`)
	s = startedArgs.ReplaceAllString(s, "headless started <id> <pid>")
	return s
}

func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \"'{}") {
			a = fmt.Sprintf("%q", a)
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

// pinFile compares got with testdata/contract/<name>.txt, or rewrites the
// file under HEADROOM_CONTRACT_UPDATE=1.
func pinFile(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "contract", name+".txt")
	if updatePins {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (HEADROOM_CONTRACT_UPDATE=1 records it)", name, err)
	}
	if string(want) != got {
		t.Errorf("%s changed:\n%s", name, lineDiff(string(want), got))
	}
}

// lineDiff lists the lines only one side has, in order, enough to find a
// changed contract without a diff tool.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for _, l := range w {
		if !slices.Contains(g, l) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	for _, l := range g {
		if !slices.Contains(w, l) {
			fmt.Fprintf(&b, "+ %s\n", l)
		}
	}
	if b.Len() == 0 {
		return "the same lines in another order or count:\n--- want\n" + want + "--- got\n" + got
	}
	return b.String()
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// built is the headroom binary, built once for every test that runs it.
var built struct {
	once      sync.Once
	dir, path string
	out       []byte
	err       error
}

func headroomBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary")
	}
	built.once.Do(func() {
		if built.dir, built.err = os.MkdirTemp("", "headroom-contract"); built.err != nil {
			return
		}
		built.path = filepath.Join(built.dir, "headroom")
		built.out, built.err = exec.Command("go", "build", "-o", built.path, ".").CombinedOutput()
	})
	if built.err != nil {
		t.Fatalf("build: %v\n%s", built.err, built.out)
	}
	return built.path
}

// TestMain clears what would re-point a run in this process at the caller's
// own machine — headroom's variables, the vendors' home and credential
// variables, a browser, an ssh session — before any test reads it. The
// contract runs set their environment whole.
// updatePins is the rewrite switch, read when the package initializes:
// TestMain clears every HEADROOM_ variable before any test runs.
var updatePins = os.Getenv("HEADROOM_CONTRACT_UPDATE") != ""

func TestMain(m *testing.M) {
	ambient := []string{"BROWSER", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"}
	for _, v := range []config.Vendor{config.Claude, config.Codex} {
		ambient = append(ambient, v.EnvPolicy().Strip...)
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "HEADROOM_") {
			ambient = append(ambient, name)
		}
	}
	for _, v := range ambient {
		os.Unsetenv(v)
	}
	code := m.Run()
	if built.dir != "" {
		os.RemoveAll(built.dir)
	}
	os.Exit(code)
}
