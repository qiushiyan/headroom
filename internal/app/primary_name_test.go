package app

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/state"
)

const tokenA = "sk-ant-oat01-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// renameHome is a home whose records were written while its primary,
// logged in as qiushi@planlab.ai, was named "qiushi": `.current`, a
// reservation, a headless token, two re-homes (one the primary's, one an
// extra's), and its --last. An extra, yqs@planlab.ai, sits beside it.
func renameHome(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	cfg := config.ForHome(home)
	root := cfg.Claude.AccountsRoot
	extra := filepath.Join(root, "yqs@planlab.ai")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Claude.PrimaryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.Claude.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"qiushi@planlab.ai","accountUuid":"u-q"}}`)
	writeJSON(t, filepath.Join(extra, ".claude.json"), `{"oauthAccount":{"emailAddress":"yqs@planlab.ai","accountUuid":"u-y"}}`)
	writeJSON(t, cfg.Claude.CurrentFile(), "qiushi\n")
	if err := os.WriteFile(cfg.Claude.ReservedFile(), []byte("qiushi\t/work/a\nyqs@planlab.ai\t/work/b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := headless.Put(root, "qiushi", headless.Token{Value: tokenA, MintedMS: 5, Key: "uuid:u-q"}); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"version": 1,
		"sessions": map[string]any{
			"s-primary": map[string]any{"account": "qiushi", "atMs": 1},
			"s-extra":   map[string]any{"account": "yqs@planlab.ai", "atMs": 2},
		},
		"placements": map[string]any{
			"recent":  []any{map[string]any{"account": "uuid:u-q", "name": "qiushi", "pid": 1, "at_ms": 3, "home": root}},
			"by_home": map[string]any{root: map[string]any{"name": "qiushi", "at_ms": 3}},
		},
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Claude.PrimaryName = "qiushi@planlab.ai"
	return cfg
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A host file that renames the primary is all a person changes: the next run
// moves every record written under the old name — and nothing of the extra's
// — and the moved records resolve.
func TestARenamedPrimaryTakesItsRecords(t *testing.T) {
	cfg := renameHome(t)
	var out bytes.Buffer
	SettlePrimaryNames(cfg, &out)
	if !strings.Contains(out.String(), "Claude Code's primary is now qiushi@planlab.ai; moved its records from qiushi") {
		t.Errorf("note: %q", out.String())
	}

	scope := cfg.Claude
	if got := readFile(t, scope.CurrentFile()); got != "qiushi@planlab.ai\n" {
		t.Errorf(".current: %q", got)
	}
	held, err := accounts.ReadReservations(scope)
	if err != nil || !held.Admits("qiushi@planlab.ai", "/work/a") || held.Held("qiushi") || !held.Admits("yqs@planlab.ai", "/work/b") {
		t.Errorf("reservations: %v %v", held, err)
	}
	tokens, err := headless.Read(scope.AccountsRoot)
	if err != nil || tokens["qiushi@planlab.ai"].Value != tokenA || len(tokens) != 1 {
		t.Errorf("tokens: %v %v", tokens, err)
	}
	st := state.Open(scope)
	snap := st.Load()
	if o, _ := snap.Owner("s-primary"); o.Account != "qiushi@planlab.ai" {
		t.Errorf("the primary's re-home: %+v", o)
	}
	if o, _ := snap.Owner("s-extra"); o.Account != "yqs@planlab.ai" {
		t.Errorf("the extra's re-home moved: %+v", o)
	}
	led := snap.Placements()
	if last, ok := led.Newest(scope.AccountsRoot); !ok || last.Name != "qiushi@planlab.ai" {
		t.Errorf("--last: %+v %v", last, ok)
	}
	if led.Recent[0].Name != "qiushi@planlab.ai" {
		t.Errorf("recent launch: %+v", led.Recent[0])
	}
	if got := st.RecordedPrimary(); got != "qiushi@planlab.ai" {
		t.Errorf("recorded: %q", got)
	}

	set := accounts.Discover(scope)
	if a, err := set.Select(""); err != nil || !a.IsPrimary() || a.Name != "qiushi@planlab.ai" {
		t.Errorf(".current resolves to %+v (%v)", a, err)
	}
}

// The second run finds the name recorded and touches nothing.
func TestASettledPrimaryIsLeftAlone(t *testing.T) {
	cfg := renameHome(t)
	SettlePrimaryNames(cfg, &bytes.Buffer{})
	statePath := filepath.Join(cfg.Claude.AccountsRoot, "state.json")
	before := readFile(t, statePath)
	var out bytes.Buffer
	SettlePrimaryNames(cfg, &out)
	if out.Len() != 0 {
		t.Errorf("a second run said %q", out.String())
	}
	if after := readFile(t, statePath); after != before {
		t.Errorf("a second run rewrote state.json")
	}
}

// Runs that start together move the records once: one says so, and the rest
// find them moved.
func TestConcurrentRunsMoveThePrimaryOnce(t *testing.T) {
	cfg := renameHome(t)
	const runs = 6
	outs := make([]bytes.Buffer, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Go(func() {
			SettlePrimaryNames(cfg, &outs[i])
		})
	}
	wg.Wait()
	notes := 0
	for i := range outs {
		notes += strings.Count(outs[i].String(), "moved its records")
		if strings.Contains(outs[i].String(), "could not be moved") {
			t.Errorf("run %d: %q", i, outs[i].String())
		}
	}
	if notes != 1 {
		t.Errorf("%d runs said they moved the records", notes)
	}
	if o, _ := state.Open(cfg.Claude).Load().Owner("s-primary"); o.Account != "qiushi@planlab.ai" {
		t.Errorf("the re-home: %+v", o)
	}
}

// Codex's primary is renamed the same way, from the local part of the email
// its auth.json is logged in as.
func TestARenamedCodexPrimaryTakesItsRecords(t *testing.T) {
	home := t.TempDir()
	cfg := config.ForHome(home)
	scope := cfg.Codex
	codexauthtest.Login{Email: "qiushi@planlab.ai", AccountID: "acc-q", UserID: "user-q"}.Write(t, scope.PrimaryDir())
	if err := os.MkdirAll(scope.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, scope.CurrentFile(), "qiushi\n")
	cfg.Codex.PrimaryName = "qiushi@planlab.ai"
	var out bytes.Buffer
	SettlePrimaryNames(cfg, &out)
	if !strings.Contains(out.String(), "Codex's primary is now qiushi@planlab.ai; moved its records from qiushi") {
		t.Errorf("note: %q", out.String())
	}
	if got := readFile(t, scope.CurrentFile()); got != "qiushi@planlab.ai\n" {
		t.Errorf(".current: %q", got)
	}
	if got := state.Open(scope).RecordedPrimary(); got != "qiushi@planlab.ai" {
		t.Errorf("recorded: %q", got)
	}
	if a, err := accounts.Discover(cfg.Codex).Select(""); err != nil || !a.IsPrimary() || a.Name != "qiushi@planlab.ai" {
		t.Errorf(".current resolves to %+v (%v)", a, err)
	}
}

// A name an extra's dir already has would hand the primary's records to that
// account: nothing moves, and the run says why.
func TestAPrimaryNamedLikeAnExtraMovesNothing(t *testing.T) {
	cfg := renameHome(t)
	cfg.Claude.PrimaryName = "yqs@planlab.ai"
	var out bytes.Buffer
	SettlePrimaryNames(cfg, &out)
	if !strings.Contains(out.String(), "also an extra account's dir") {
		t.Errorf("note: %q", out.String())
	}
	if got := readFile(t, cfg.Claude.CurrentFile()); got != "qiushi\n" {
		t.Errorf(".current: %q", got)
	}
	if got := state.Open(cfg.Claude).RecordedPrimary(); got != "" {
		t.Errorf("recorded: %q", got)
	}
}

// A reservation file that does not read is a person's statement nobody can
// see: nothing moves — `.current` included — nothing is recorded, and the next
// run tries again.
func TestUnreadableReservationsMoveNothing(t *testing.T) {
	cfg := renameHome(t)
	if err := os.WriteFile(cfg.Claude.ReservedFile(), []byte("not a reservation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	SettlePrimaryNames(cfg, &out)
	if !strings.Contains(out.String(), "could not be moved") {
		t.Errorf("note: %q", out.String())
	}
	if got := readFile(t, cfg.Claude.CurrentFile()); got != "qiushi\n" {
		t.Errorf(".current moved: %q", got)
	}
	st := state.Open(cfg.Claude)
	if o, _ := st.Load().Owner("s-primary"); o.Account != "qiushi" || st.RecordedPrimary() != "" {
		t.Errorf("re-home %+v, recorded %q", o, st.RecordedPrimary())
	}
}

// An unpinned name follows the login and is not settled; a pinned one in a
// home with nothing yet writes no file just to record it.
func TestNothingToSettleWritesNothing(t *testing.T) {
	cfg := renameHome(t)
	cfg.Claude.PrimaryName = ""
	SettlePrimaryNames(cfg, &bytes.Buffer{})
	if got := readFile(t, cfg.Claude.CurrentFile()); got != "qiushi\n" {
		t.Errorf("an unpinned name moved .current: %q", got)
	}

	fresh := config.ForHome(t.TempDir())
	fresh.Claude.PrimaryName = "qiushi@planlab.ai"
	var out bytes.Buffer
	SettlePrimaryNames(fresh, &out)
	if _, err := os.Stat(filepath.Join(fresh.Claude.AccountsRoot, "state.json")); err == nil || out.Len() != 0 {
		t.Errorf("a fresh home got a state file (%v) or a note %q", err, out.String())
	}
}

// Every command takes its configuration through the one provider, which
// settles the names first: a real run moves the records, says so on stderr,
// and resolves the primary by its new name in the same run.
func TestARunSettlesThePrimaryBeforeItsCommand(t *testing.T) {
	bin := headroomBinary(t)
	cfg := renameHome(t)
	cmd := exec.Command(bin, "resolve", "qiushi@planlab.ai")
	cmd.Env = append(os.Environ(), "HEADROOM_HOME="+cfg.Claude.Home, "HEADROOM_PRIMARY_NAME=qiushi@planlab.ai")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("resolve: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "qiushi@planlab.ai\t") || !strings.HasSuffix(strings.TrimSpace(stdout.String()), "primary") {
		t.Errorf("stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "moved its records from qiushi") {
		t.Errorf("stderr: %q", stderr.String())
	}
	if got := readFile(t, cfg.Claude.CurrentFile()); got != "qiushi@planlab.ai\n" {
		t.Errorf(".current: %q", got)
	}
}
