package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMunge(t *testing.T) {
	if got := Munge("/Users/q/dev/.worktrees/x"); got != "-Users-q-dev--worktrees-x" {
		t.Errorf("Munge = %q", got)
	}
}

func TestParseTailTitleChain(t *testing.T) {
	storeDir := "-tmp-proj"
	lines := []string{
		`{"type":"user","sessionId":"s1","cwd":"/tmp/proj","gitBranch":"main","message":{"role":"user","content":"first prompt"}}`,
		`{"type":"ai-title","aiTitle":"old title","sessionId":"s1"}`,
		`{"type":"ai-title","aiTitle":"new title","sessionId":"s1"}`,
		`{"type":"last-prompt","lastPrompt":"do the thing","sessionId":"s1"}`,
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","content":[{"type":"text","text":"done"},{"type":"tool_use"}]}}`,
	}
	tail := ParseTail([]byte(strings.Join(lines, "\n")+"\n"), storeDir, true)
	if tail.ID != "s1" || tail.AITitle != "new title" || tail.Title() != "new title" {
		t.Errorf("ai-title chain: %+v", tail)
	}
	if tail.CWD != "/tmp/proj" || tail.Branch != "main" {
		t.Errorf("cwd/branch: %+v", tail)
	}
	if tail.LastPrompt != "do the thing" || tail.LastReply != "done" || tail.LastUser != "first prompt" {
		t.Errorf("preview fields: %+v", tail)
	}

	// A rename outranks the generated title no matter the order.
	withRename := strings.Join(lines, "\n") + "\n" +
		`{"type":"custom-title","customTitle":"my name","sessionId":"s1"}` + "\n"
	if got := ParseTail([]byte(withRename), storeDir, true).Title(); got != "my name" {
		t.Errorf("custom-title should win: %q", got)
	}
}

func TestParseTailCwdVerifiedNotDemunged(t *testing.T) {
	// The transcript visits several cwds; only the one whose munge equals the
	// store dir name is the resume target — the newest such value wins, and a
	// last cwd from somewhere else entirely (measured in the live store) must
	// not leak into it.
	storeDir := "-repo-wt-a"
	lines := []string{
		`{"type":"user","cwd":"/repo/wt/a","message":{"role":"user","content":"x"}}`,
		`{"type":"user","cwd":"/repo/other","message":{"role":"user","content":"y"}}`,
	}
	tail := ParseTail([]byte(strings.Join(lines, "\n")+"\n"), storeDir, true)
	if tail.CWD != "/repo/wt/a" {
		t.Errorf("CWD = %q, want the munge-verified value", tail.CWD)
	}
	// relocated records participate under the same verification.
	reloc := `{"type":"relocated","relocatedCwd":"/repo/wt/a","sessionId":"s"}` + "\n"
	if got := ParseTail([]byte(reloc), storeDir, true).CWD; got != "/repo/wt/a" {
		t.Errorf("relocatedCwd = %q", got)
	}
	// No verifiable cwd at all → empty, never a guess.
	if got := ParseTail([]byte(lines[1]+"\n"), storeDir, true).CWD; got != "" {
		t.Errorf("unverifiable cwd should stay empty, got %q", got)
	}
}

func TestParseTailDegradation(t *testing.T) {
	storeDir := "-p"
	// Torn final line: skipped, not drift. Mid-file garbage: counted Bad.
	data := "garbage-not-json\n" +
		`{"type":"ai-title","aiTitle":"t","sessionId":"s"}` + "\n" +
		`{"type":"ai-title","aiTitle":"torn`
	tail := ParseTail([]byte(data), storeDir, true)
	if tail.Bad != 1 || tail.AITitle != "t" {
		t.Errorf("degradation: %+v", tail)
	}
	// A last-prompt record without its field must not erase an earlier value.
	data = `{"type":"last-prompt","lastPrompt":"kept","sessionId":"s"}` + "\n" +
		`{"type":"last-prompt","leafUuid":"l","sessionId":"s"}` + "\n"
	if got := ParseTail([]byte(data), storeDir, true).LastPrompt; got != "kept" {
		t.Errorf("nil lastPrompt erased value: %q", got)
	}
	// Suffix mode drops the leading partial line.
	data = `alf-line"}` + "\n" + `{"type":"ai-title","aiTitle":"t2","sessionId":"s"}` + "\n"
	tail = ParseTail([]byte(data), storeDir, false)
	if tail.AITitle != "t2" || tail.Bad != 0 {
		t.Errorf("suffix mode: %+v", tail)
	}
}

func TestParseHistory(t *testing.T) {
	// The decoded sessionId field decides — a different session's UUID inside
	// a prompt body (the live store has these, one cross-account) must not
	// attribute, and a torn final line during a concurrent write is skipped.
	input := `{"display":"look at 11111111-aaaa-bbbb-cccc-dddddddddddd please","sessionId":"22222222-aaaa-bbbb-cccc-dddddddddddd","timestamp":100}
{"display":"x","sessionId":"22222222-aaaa-bbbb-cccc-dddddddddddd","timestamp":300}
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
	probe := func(pid int) (int64, bool) { return 1_785_700_000, true }
	live := []RegistryEntry{{Account: "b", SessionID: "s", PID: 1, StartedAtMS: 1_785_700_003_000, OK: true}}

	cases := []struct {
		name      string
		registry  []RegistryEntry
		owners    map[string]OwnerRec
		hists     map[string]History
		wantAcct  string
		wantState OwnerState
	}{
		{"no evidence", nil, nil, nil, "", OwnerNone},
		{"history only", nil, nil,
			hist(map[string]map[string]int64{"a": {"s": 100}}), "a", OwnerHistory},
		{"newest history wins", nil, nil,
			hist(map[string]map[string]int64{"a": {"s": 100}, "b": {"s": 200}}), "b", OwnerHistory},
		{"rehome beats older history", nil,
			map[string]OwnerRec{"s": {Account: "b", AtMS: 300}},
			hist(map[string]map[string]int64{"a": {"s": 100}}), "b", OwnerRehome},
		{"newer prompt beats older rehome", nil,
			map[string]OwnerRec{"s": {Account: "b", AtMS: 100}},
			hist(map[string]map[string]int64{"a": {"s": 200}}), "a", OwnerHistory},
		{"verified live outranks everything", live,
			map[string]OwnerRec{"s": {Account: "a", AtMS: 9999}}, nil, "b", OwnerLive},
		{"rehome to deleted account surfaces, never falls through", nil,
			map[string]OwnerRec{"s": {Account: "gone", AtMS: 300}},
			hist(map[string]map[string]int64{"a": {"s": 100}}), "", OwnerMissing},
		{"same-instant history claims are undecidable", nil, nil,
			hist(map[string]map[string]int64{"a": {"s": 100}, "b": {"s": 100}}), "", OwnerConflict},
		{"rehome breaks a same-instant tie", nil,
			map[string]OwnerRec{"s": {Account: "b", AtMS: 100}},
			hist(map[string]map[string]int64{"a": {"s": 100}}), "b", OwnerRehome},
	}
	for _, c := range cases {
		acct, st := resolveOwner("s", c.registry, probe, c.owners, c.hists, names)
		if acct != c.wantAcct || st != c.wantState {
			t.Errorf("%s: = (%q, %v), want (%q, %v)", c.name, acct, st, c.wantAcct, c.wantState)
		}
	}
}

func TestLiveness(t *testing.T) {
	const startMS = 1_785_700_000_000
	entries := []RegistryEntry{
		{Account: "a", SessionID: "live", PID: 10, StartedAtMS: startMS, OK: true},
		{Account: "a", SessionID: "recycled", PID: 11, StartedAtMS: startMS, OK: true},
		{Account: "a", SessionID: "dead", PID: 12, StartedAtMS: startMS, OK: true},
		{Account: "a", SessionID: "mystery", PID: 13, OK: false},
	}
	probe := func(pid int) (int64, bool) {
		switch pid {
		case 10:
			return startMS/1000 + 2, true // within tolerance: vendor stamp vs kernel
		case 11:
			return startMS/1000 + 86_400, true // pid reused a day later
		default:
			return 0, false
		}
	}
	m := Liveness(entries, probe)
	if m["live"] != Live {
		t.Errorf("live = %v", m["live"])
	}
	if m["recycled"] != NotLive {
		t.Errorf("recycled pid must not read live: %v", m["recycled"])
	}
	if m["dead"] != NotLive {
		t.Errorf("dead = %v", m["dead"])
	}
	if m["mystery"] != LiveUnknown {
		t.Errorf("unverifiable claim must stay unknown: %v", m["mystery"])
	}
}

func TestOwnersStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".owners")
	now := time.UnixMilli(1000)
	if err := ReHome(path, "s1", "a@x.com", now, nil); err != nil {
		t.Fatal(err)
	}
	if err := ReHome(path, "s2", "b@x.com", now, nil); err != nil {
		t.Fatal(err)
	}
	m := LoadOwners(path)
	if m["s1"].Account != "a@x.com" || m["s2"].AtMS != 1000 {
		t.Fatalf("LoadOwners = %v", m)
	}
	// GC drops what keep rejects; the entry being written survives.
	if err := ReHome(path, "s3", "c@x.com", now, func(id string) bool { return id == "s3" }); err != nil {
		t.Fatal(err)
	}
	m = LoadOwners(path)
	if len(m) != 1 || m["s3"].Account != "c@x.com" {
		t.Fatalf("after GC = %v", m)
	}
	if err := ForgetOwner(path, "s3"); err != nil {
		t.Fatal(err)
	}
	if m = LoadOwners(path); len(m) != 0 {
		t.Fatalf("after forget = %v", m)
	}
	// Corrupt file = empty store, and the next write recovers it.
	os.WriteFile(path, []byte("{broken"), 0o644)
	if m = LoadOwners(path); len(m) != 0 {
		t.Fatal("corrupt file must read as empty")
	}
	if err := ReHome(path, "s4", "d@x.com", now, nil); err != nil {
		t.Fatal(err)
	}
	if m = LoadOwners(path); m["s4"].Account != "d@x.com" {
		t.Fatal("write over corrupt file must recover")
	}
}

// storeFixture builds a fake HEADROOM_HOME-style projects tree.
func storeFixture(t *testing.T) (projects string, write func(dirName, fileName, content string, mtime time.Time)) {
	t.Helper()
	projects = filepath.Join(t.TempDir(), "projects")
	write = func(dirName, fileName, content string, mtime time.Time) {
		dir := filepath.Join(projects, dirName)
		os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, fileName)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	return projects, write
}

func rec(id, cwd, title string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"message":{"role":"user","content":"p"}}
{"type":"ai-title","aiTitle":%q,"sessionId":%q}
`, id, cwd, title, id)
}

func TestCollectDedupesOrphans(t *testing.T) {
	projects, write := storeFixture(t)
	tm := time.Now().Add(-time.Hour)
	write("-tmp-p", "aaaa-1.jsonl", rec("aaaa-1", "/tmp/p", "real"), tm)
	write("-tmp-p", "aaaa-1.orphaned-123-ab.jsonl", rec("aaaa-1", "/tmp/p", "orphan"), tm.Add(time.Minute))
	l := Collect(Input{ProjectsDir: projects, CWD: "/nowhere"})
	if len(l.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(l.Sessions))
	}
	if l.Sessions[0].Tail.Title() != "real" {
		t.Errorf("canonical file must win over orphan: %q", l.Sessions[0].Tail.Title())
	}
}

func TestCollectGroupsAndSorts(t *testing.T) {
	home := t.TempDir()
	// A fake repo: main checkout + a worktree whose .git file points at it.
	repo := filepath.Join(home, "dev", "proj")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	wt := filepath.Join(home, "dev", ".worktrees", "proj", "feat")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"),
		[]byte("gitdir: "+repo+"/.git/worktrees/feat\n"), 0o644)
	elsewhere := filepath.Join(home, "other")
	os.MkdirAll(elsewhere, 0o755)

	projects, write := storeFixture(t)
	now := time.Now()
	write(Munge(repo), "s-main.jsonl", rec("s-main", repo, "in main"), now.Add(-3*time.Hour))
	write(Munge(wt), "s-wt.jsonl", rec("s-wt", wt, "in worktree"), now.Add(-1*time.Hour))
	write(Munge(elsewhere), "s-other.jsonl", rec("s-other", elsewhere, "elsewhere"), now.Add(-2*time.Hour))
	write("-gone-dir", "s-dead.jsonl", rec("s-dead", "/gone/dir", "dead dir"), now.Add(-4*time.Hour))

	l := Collect(Input{ProjectsDir: projects, CWD: repo})
	if len(l.Sessions) != 4 {
		t.Fatalf("want 4 sessions, got %d", len(l.Sessions))
	}
	byID := map[string]*Session{}
	for i, s := range l.Sessions {
		byID[s.ID] = s
		if i > 0 && s.MTime.After(l.Sessions[i-1].MTime) {
			t.Error("sessions must sort newest first")
		}
	}
	// Worktree sessions are local to the repo — the whole point of the rule.
	if !byID["s-main"].Local || !byID["s-wt"].Local {
		t.Errorf("main/worktree must both be local: %+v %+v", byID["s-main"], byID["s-wt"])
	}
	if byID["s-other"].Local {
		t.Error("unrelated project must not be local")
	}
	// A deleted project dir: global, not resumable, never a guess.
	dead := byID["s-dead"]
	if dead.Local || dead.DirOK || dead.RepoKey != "" {
		t.Errorf("dead dir must be global and unresumable: %+v", dead)
	}
	if !byID["s-main"].DirOK || byID["s-wt"].RepoKey != byID["s-main"].RepoKey {
		t.Errorf("repo identity: main=%+v wt=%+v", byID["s-main"], byID["s-wt"])
	}
}

func TestDeleteTranscript(t *testing.T) {
	projects, write := storeFixture(t)
	tm := time.Now()
	write("-tmp-p", "bbbb-2.jsonl", rec("bbbb-2", "/tmp/p", "x"), tm)
	write("-tmp-p", "bbbb-2.orphaned-9-zz.jsonl", "", tm)
	closure := filepath.Join(projects, "-tmp-p", "bbbb-2", "tool-results")
	os.MkdirAll(closure, 0o755)
	os.WriteFile(filepath.Join(closure, "r.json"), []byte("{}"), 0o644)
	write("-tmp-p", "cccc-3.jsonl", rec("cccc-3", "/tmp/p", "keep"), tm)

	l := Collect(Input{ProjectsDir: projects, CWD: "/nowhere"})
	var target *Session
	for _, s := range l.Sessions {
		if s.ID == "bbbb-2" {
			target = s
		}
	}
	if err := DeleteTranscript(target); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(filepath.Join(projects, "-tmp-p"))
	if len(left) != 1 || left[0].Name() != "cccc-3.jsonl" {
		names := []string{}
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("after delete: %v", names)
	}
	// An id that could escape the store dir is refused outright.
	if err := DeleteTranscript(&Session{ID: "../evil", StoreDir: projects}); err == nil {
		t.Error("path-escaping id must be refused")
	}
}
