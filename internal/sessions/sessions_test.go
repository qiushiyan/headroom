package sessions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/headroom/internal/tag"
)

func TestParseHistory(t *testing.T) {
	// The decoded sessionId field decides — a different session's UUID inside
	// a prompt body (the live store has these, one cross-account) must not
	// attribute, and a torn final line during a concurrent write is skipped.
	// The newer prompt comes first: the newest claim wins, not the last line.
	input := `{"display":"x","sessionId":"22222222-aaaa-bbbb-cccc-dddddddddddd","timestamp":300}
{"display":"look at 11111111-aaaa-bbbb-cccc-dddddddddddd please","sessionId":"22222222-aaaa-bbbb-cccc-dddddddddddd","timestamp":100}
{"display":"y","sessionId":"33333333-aaaa-bbbb-cccc-dddddddddddd","timestamp":200}
not json at all
{"display":"torn","sessionId":"44444444-aaaa-bbbb-cccc-dddd`
	h := ParseHistory(strings.NewReader(input))
	if len(h.Newest) != 2 {
		t.Fatalf("Newest = %v", h.Newest)
	}
	if h.Newest["22222222-aaaa-bbbb-cccc-dddddddddddd"] != 300 {
		t.Errorf("newest wins: %v", h.Newest)
	}
	if _, ok := h.Newest["11111111-aaaa-bbbb-cccc-dddddddddddd"]; ok {
		t.Error("UUID inside a prompt body must not attribute")
	}
	if _, ok := h.Newest["44444444-aaaa-bbbb-cccc-dddddddddddd"]; ok {
		t.Error("torn final line must not attribute")
	}
	if h.Bad != 1 {
		t.Errorf("Bad = %d, want 1 (the complete non-JSON line)", h.Bad)
	}
}

func TestResolveOwner(t *testing.T) {
	names := map[string]bool{"a": true, "b": true}
	hist := func(m map[string]map[string]int64) map[string]History {
		out := map[string]History{}
		for acct, ids := range m {
			out[acct] = History{Newest: ids}
		}
		return out
	}

	cases := []struct {
		name     string
		owners   map[string]OwnerRec
		hists    map[string]History
		wantAcct string
	}{
		{"no evidence", nil, nil, ""},
		{"history only", nil,
			hist(map[string]map[string]int64{"a": {"s": 100}}), "a"},
		{"newest history wins", nil,
			hist(map[string]map[string]int64{"a": {"s": 100}, "b": {"s": 200}}), "b"},
		{"rehome beats older history",
			map[string]OwnerRec{"s": {Account: "b", AtMS: 300}},
			hist(map[string]map[string]int64{"a": {"s": 100}}), "b"},
		{"newer prompt beats older rehome",
			map[string]OwnerRec{"s": {Account: "b", AtMS: 100}},
			hist(map[string]map[string]int64{"a": {"s": 200}}), "a"},
		{"rehome to deleted account surfaces, never falls through",
			map[string]OwnerRec{"s": {Account: "gone", AtMS: 300}},
			hist(map[string]map[string]int64{"a": {"s": 100}}), ""},
		{"same-instant history claims are undecidable", nil,
			hist(map[string]map[string]int64{"a": {"s": 100}, "b": {"s": 100}}), ""},
		{"same-instant rehome vs history is a conflict too",
			map[string]OwnerRec{"s": {Account: "b", AtMS: 100}},
			hist(map[string]map[string]int64{"a": {"s": 100}}), ""},
	}
	for _, c := range cases {
		byRef := map[string]string{}
		for n := range names {
			byRef[n] = n
		}
		if acct := newestClaim("s", byRef, c.hists, c.owners); acct != c.wantAcct {
			t.Errorf("%s: = %q, want %q", c.name, acct, c.wantAcct)
		}
	}

	// A verified live registry entry outranks everything: that account is
	// driving the session at this instant, whatever the re-homes and prompts.
	accts := []AccountRef{{Name: "a", Ref: "a", Dir: t.TempDir()}, {Name: "b", Ref: "b", Dir: t.TempDir()}}
	newer := map[string]OwnerRec{"s": {Account: "a", AtMS: 9999}}
	if got := OwnerOf("s", accts, ProcessEvidence{State: Live, Account: "b"}, newer); got != "b" {
		t.Errorf("verified live: = %q, want b", got)
	}
	if got := OwnerOf("s", accts, ProcessEvidence{}, newer); got != "a" {
		t.Errorf("with nothing live: = %q, want the re-home's a", got)
	}
}

// A directory named .git that holds no HEAD is not a checkout. Worktree
// tooling and editors leave these husks behind — the live store has one at
// dev/itell/apps/platform holding nothing but info/exclude — and stopping the
// walk there hides the real repository above it: work in it gets keyed on a
// path git does not consider a checkout, and a reservation for the
// repository would not reach it.
func TestRepoKeyWalksPastAGitHusk(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("repo/.git/HEAD", "ref: refs/heads/feat/summary-attempt-policy\n")
	write("repo/apps/platform/.git/info/exclude", "")
	if got := RepoKey(filepath.Join(root, "repo/apps/platform")); got != filepath.Join(root, "repo") {
		t.Errorf("RepoKey = %q, want the real checkout above the husk", got)
	}

	// With no real repo above it, the husk is still the best evidence there
	// is — the walk falls back to it, not to the directory asked about.
	write("orphan/.git/info/exclude", "")
	write("orphan/sub/file", "")
	if got := RepoKey(filepath.Join(root, "orphan/sub")); got != filepath.Join(root, "orphan") {
		t.Errorf("under a husk with nothing above it: %q, want the husk's directory", got)
	}
}

func TestRegistryProblemsKeepTheirScope(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	os.Mkdir(dir, 0755)
	os.WriteFile(filepath.Join(dir, "unknown.json"), []byte(`broken`), 0644)
	reg := ReadRegistry("a", root)
	if len(reg.Problems) != 1 || len(reg.Entries) != 0 {
		t.Fatalf("lost registry problem: %+v", reg)
	}
	os.WriteFile(filepath.Join(dir, "known.json"), []byte(`{"sessionId":"known","pid":7}`), 0644)
	evidence, _ := InspectClaims(ReadRegistry("a", root).Entries, nil)
	if got := evidence["unrelated"].State; got != NotLive {
		t.Fatalf("nameless file attributed to unrelated session: %v", got)
	}
	if got := evidence["known"].State; got != LiveUnknown {
		t.Fatalf("damaged identifiable claim = %v", got)
	}
	os.Chmod(dir, 0)
	defer os.Chmod(dir, 0755)
	if !ReadRegistry("a", root).Unreadable {
		t.Fatal("an unreadable directory read as an empty registry")
	}
}

func TestRegistryCarriesTheVendorsStatus(t *testing.T) {
	dir := t.TempDir()
	sdir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := func(pid int, status string) {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":"s%d","startedAt":1785700003000%s}`, pid, pid, status)
		if err := os.WriteFile(filepath.Join(sdir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec(1, `,"status":"busy"`)
	rec(2, `,"status":"idle"`)
	rec(3, `,"status":"shell"`)
	rec(4, ``)
	rec(5, `,"status":7`)
	rec(6, `,"status":null`)
	rec(7, `,"status":""`)

	want := map[int]struct {
		status string
		state  tag.State
		busy   bool
	}{
		1: {"busy", tag.OK, true},
		2: {"idle", tag.OK, false},
		3: {"shell", tag.OK, false}, // carried as read, never equated with idle
		4: {"", tag.None, false},
		5: {"", tag.Bad, false},
		6: {"", tag.None, false},
		7: {"", tag.Bad, false},
	}
	reg := ReadRegistry("a", dir)
	if len(reg.Entries) != len(want) || len(reg.Problems) != 0 {
		t.Fatalf("entries %d problems %v", len(reg.Entries), reg.Problems)
	}
	for _, e := range reg.Entries {
		w := want[e.PID]
		if e.Status != w.status || e.StatusState != w.state || e.Busy() != w.busy {
			t.Errorf("pid %d: %q/%v busy=%v, want %q/%v busy=%v", e.PID, e.Status, e.StatusState, e.Busy(), w.status, w.state, w.busy)
		}
	}
}

// One sample per pid decides every claim: a claim counts only when its pid
// runs and started when the claim says, a pid proven absent or since recycled
// clears it, and a failed inspection or a damaged record leaves it unknown.
// Proof outranks doubt for a session with several claims — the resume guard
// refuses only Live — and only verified claims are returned as running.
func TestInspectClaimsVerifiesEachClaimByItsOwnProcess(t *testing.T) {
	const startMS = 1_785_700_003_000
	entries := []RegistryEntry{
		{Account: "a", SessionID: "live", PID: 1, StartedAtMS: startMS, OK: true},
		// The same session under a pid another process has since taken.
		{Account: "a", SessionID: "live", PID: 2, StartedAtMS: startMS, OK: true},
		{Account: "b", SessionID: "recycled", PID: 2, StartedAtMS: startMS, OK: true},
		{Account: "b", SessionID: "registered-late", PID: 3, StartedAtMS: startMS, OK: true},
		{Account: "b", SessionID: "gone", PID: 4, StartedAtMS: startMS, OK: true},
		{Account: "b", SessionID: "uninspectable", PID: 5, StartedAtMS: startMS, OK: true},
		{Account: "b", SessionID: "torn", PID: 6, OK: false},
		{Account: "c", SessionID: "doubted", PID: 1, StartedAtMS: startMS, OK: true},
		{Account: "c", SessionID: "doubted", PID: 7, OK: false},
	}
	calls := map[int]int{}
	probe := func(pid int) (int64, error) {
		calls[pid]++
		switch pid {
		case 1:
			return startMS/1000 + 2, nil // kernel and vendor stamps a moment apart
		case 2:
			return startMS/1000 + 86_400, nil // a day later: another process
		case 3:
			return startMS/1000 - 3, nil
		case 4:
			return 0, os.ErrNotExist
		case 5:
			return 0, os.ErrPermission
		}
		return 0, errors.New("a damaged claim was probed")
	}
	evidence, live := InspectClaims(entries, probe)
	want := map[string]ProcessEvidence{
		"live":            {Live, "a"},
		"recycled":        {State: NotLive},
		"registered-late": {Live, "b"},
		"gone":            {State: NotLive},
		"uninspectable":   {State: LiveUnknown},
		"torn":            {State: LiveUnknown},
		"doubted":         {Live, "c"},
	}
	for id, w := range want {
		if evidence[id] != w {
			t.Errorf("%s: %+v, want %+v", id, evidence[id], w)
		}
	}
	for pid, n := range calls {
		if n != 1 || pid > 5 {
			t.Errorf("pid %d sampled %d time(s), want once and never for a damaged claim", pid, n)
		}
	}
	var running []string
	for _, e := range live {
		running = append(running, fmt.Sprintf("%s/%d", e.SessionID, e.PID))
	}
	if got := strings.Join(running, " "); got != "live/1 registered-late/3 doubted/1" {
		t.Errorf("running claims = %s", got)
	}
}
