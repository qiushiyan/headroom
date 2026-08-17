package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/state"
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

func TestParseTailModel(t *testing.T) {
	lines := []string{
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"a"}]}}`,
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"tool_use"}]}}`,
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"err"}]}}`,
	}
	tail := ParseTail([]byte(strings.Join(lines, "\n")+"\n"), "-tmp-proj", true)
	if tail.Model != "claude-fable-5" {
		t.Errorf("Model = %q: newest wins, tool-use-only records count, <synthetic> never does", tail.Model)
	}
	// The synthetic placeholder must not surface even when it is the only claim.
	only := `{"type":"assistant","sessionId":"s1","message":{"role":"assistant","model":"<synthetic>","content":[]}}` + "\n"
	if got := ParseTail([]byte(only), "-tmp-proj", true).Model; got != "" {
		t.Errorf("Model = %q, want empty for synthetic-only", got)
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
		{"same-instant rehome vs history is a conflict too", nil,
			map[string]OwnerRec{"s": {Account: "b", AtMS: 100}},
			hist(map[string]map[string]int64{"a": {"s": 100}}), "", OwnerConflict},
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
	// A verified-live claim beside an unverifiable one for the same session:
	// proof outranks uncertainty — Live, never LiveUnknown, or the resume
	// guard (which refuses only Live) would resume a session proven open.
	both := append(entries[:1:1], RegistryEntry{Account: "b", SessionID: "live", PID: 99, OK: false})
	if got := Liveness(both, probe)["live"]; got != Live {
		t.Errorf("verified live + unverifiable claim = %v, want Live", got)
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

// The GC invariant spans both packages — TranscriptIDs enumerates, the store
// holds the lock — so it is pinned here, where the transcript fixture lives.
//
// GC must consult the store inside the lock, at write time, never a caller's
// snapshot. The trap: picker A listed before session s-new existed; a re-home
// of s-new (by any picker) must survive A's later write, because the sweep
// sees the store as it is now, not as A saw it.
func TestOwnersGCReadsStoreAtWriteTime(t *testing.T) {
	projects, write := storeFixture(t)
	st := state.Open(t.TempDir())
	live := func() (map[string]bool, bool) { return TranscriptIDs(projects) }
	now := time.Now()
	write("-tmp-p", "s-old.jsonl", rec("s-old", "/tmp/p", "old"), now)

	// A re-home for a session that has since lost its transcript…
	if err := st.ReHome("s-gone", "a@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	// …then s-new appears (created after any earlier listing), is re-homed…
	write("-tmp-p", "s-new.jsonl", rec("s-new", "/tmp/p", "new"), now)
	if err := st.ReHome("s-new", "b@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	// …and a further write GCs: s-gone (no transcript) goes, s-new stays.
	if err := st.ReHome("s-old", "c@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	m := st.Load().Owners()
	if _, ok := m["s-gone"]; ok {
		t.Error("transcriptless record must be swept")
	}
	if m["s-new"].Account != "b@x.com" || m["s-old"].Account != "c@x.com" {
		t.Errorf("live records must survive every write: %v", m)
	}
}

// Mutations must not trust the listing's liveness snapshot: a registry claim
// that appears after collection is exactly the session dd must refuse.
func TestLiveNowSeesClaimsAfterCollect(t *testing.T) {
	home := t.TempDir()
	refs := []AccountRef{{Name: "a", Dir: home}}
	probe := func(pid int) (int64, bool) { return 5_000, true }
	if got := LiveNow("s-late", refs, probe); got != NotLive {
		t.Fatalf("empty registry = %v, want NotLive", got)
	}
	os.MkdirAll(filepath.Join(home, "sessions"), 0o755)
	os.WriteFile(filepath.Join(home, "sessions", "77.json"),
		[]byte(`{"sessionId":"s-late","pid":77,"startedAt":5001000}`), 0o644)
	if got := LiveNow("s-late", refs, probe); got != Live {
		t.Errorf("claim appearing after an earlier check = %v, want Live", got)
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

// A transcript ending in one message larger than the tail budget must still
// resolve its cwd — the window widens until a record verifies. Without this,
// a resumable session renders "dir gone" (5 real transcripts did).
func TestCollectWidensPastCwdlessTail(t *testing.T) {
	projects, write := storeFixture(t)
	dir := t.TempDir()
	filler := strings.Repeat("x", TailBudget) // one record larger than the budget
	content := rec("s-big", dir, "big session") +
		`{"type":"assistant","sessionId":"s-big","message":{"role":"assistant","content":[{"type":"text","text":"` + filler + `"}]}}` + "\n"
	write(Munge(dir), "s-big.jsonl", content, time.Now())

	l := Collect(Input{ProjectsDir: projects, CWD: "/nowhere"})
	if len(l.Sessions) != 1 {
		t.Fatalf("sessions = %d", len(l.Sessions))
	}
	s := l.Sessions[0]
	if s.CWD != dir || !s.DirOK {
		t.Errorf("cwd starved by an oversized tail: CWD=%q DirOK=%v", s.CWD, s.DirOK)
	}
	if s.Tail.Title() != "big session" {
		t.Errorf("title = %q", s.Tail.Title())
	}
}

// The model must be part of the adaptive reader's contract like title and
// cwd: a tail window that starts inside one enormous assistant record skips
// it as a partial line, and a later small cwd record must not stop the
// widening while the session's only model claim sits unread.
func TestCollectWidensPastModellessTail(t *testing.T) {
	projects, write := storeFixture(t)
	dir := t.TempDir()
	filler := strings.Repeat("x", TailBudget)
	content := rec("s-model", dir, "model session") +
		`{"type":"assistant","sessionId":"s-model","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"` + filler + `"}]}}` + "\n" +
		`{"type":"user","sessionId":"s-model","cwd":` + fmt.Sprintf("%q", dir) + `,"message":{"role":"user","content":"after"}}` + "\n"
	write(Munge(dir), "s-model.jsonl", content, time.Now())

	l := Collect(Input{ProjectsDir: projects, CWD: "/nowhere"})
	if len(l.Sessions) != 1 {
		t.Fatalf("sessions = %d", len(l.Sessions))
	}
	s := l.Sessions[0]
	if s.CWD != dir {
		t.Errorf("CWD = %q", s.CWD)
	}
	if s.Tail.Model != "claude-fable-5" {
		t.Errorf("Model = %q: cwd resolving must not end the search for the model", s.Tail.Model)
	}
}

// A store walk that fails anywhere concludes nothing: one unreadable project
// dir must suspend GC entirely, not sweep the re-homes whose transcripts
// live inside it.
func TestOwnersGCSkipsOnPartialEnumeration(t *testing.T) {
	projects, write := storeFixture(t)
	st := state.Open(t.TempDir())
	live := func() (map[string]bool, bool) { return TranscriptIDs(projects) }
	now := time.Now()
	write("-p-hidden", "s-hidden.jsonl", rec("s-hidden", "/p/hidden", "hidden"), now)
	write("-p-open", "s-open.jsonl", rec("s-open", "/p/open", "open"), now)
	if err := st.ReHome("s-hidden", "a@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(projects, "-p-hidden")
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(hidden, 0o755) })
	if err := st.ReHome("s-open", "b@x.com", now, live); err != nil {
		t.Fatal(err)
	}
	m := st.Load().Owners()
	if m["s-hidden"].Account != "a@x.com" {
		t.Errorf("partial enumeration must not GC: %v", m)
	}
}

// The live half of a row's branch label. Every case here is a shape seen in
// the real store: a main checkout, a linked worktree, a submodule spelling
// its gitdir relative, a rebase in flight, and the several ways HEAD gives
// no branch at all — which must degrade to HeadUnknown so the label layer
// can fall back rather than invent.
func TestResolveReadsLiveHead(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(path string) {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A main checkout, with a nested subdir the walk has to climb out of.
	write("main/.git/HEAD", "ref: refs/heads/develop\n")
	mkdir("main/internal/app")

	// A linked worktree: .git is a file, HEAD lives in the main repo.
	write("wt/.git", "gitdir: "+filepath.Join(root, "main/.git/worktrees/wt")+"\n")
	write("main/.git/worktrees/wt/HEAD", "ref: refs/heads/feat/live-branch\n")

	// A worktree mid-rebase: HEAD is detached, the state dir names the branch.
	write("rebasing/.git", "gitdir: "+filepath.Join(root, "main/.git/worktrees/rebasing")+"\n")
	write("main/.git/worktrees/rebasing/HEAD", "fdc9b6240d1e4c8a9b3f5e7d2c1a0b8f6e4d3c2b\n")
	write("main/.git/worktrees/rebasing/rebase-merge/head-name", "refs/heads/topic\n")

	// A rebase replaying a detached HEAD: in flight, but no branch to name.
	write("rebase-anon/.git/HEAD", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	write("rebase-anon/.git/rebase-apply/head-name", "detached HEAD\n")

	// A plain detached checkout.
	write("detached/.git/HEAD", "0123456789abcdef0123456789abcdef01234567\n")

	// A submodule: gitdir relative, and no /.git/worktrees/ in the path, so
	// it is its own key — but its HEAD is still readable.
	write("super/sub/.git", "gitdir: ../.git/modules/sub\n")
	write("super/.git/modules/sub/HEAD", "ref: refs/heads/submodule-branch\n")

	// HEAD shapes that name nothing: a symref outside refs/heads, junk, and
	// a .git file pointing at a gitdir that does not exist.
	write("symref/.git/HEAD", "ref: refs/remotes/origin/main\n")
	write("junk/.git/HEAD", "not a sha and not a ref\n")
	write("dangling/.git", "gitdir: "+filepath.Join(root, "nowhere")+"\n")

	cases := []struct {
		name   string
		dir    string
		kind   HeadKind
		branch string
		commit string
	}{
		{"main checkout", "main", HeadBranch, "develop", ""},
		{"subdir climbs to the checkout", "main/internal/app", HeadBranch, "develop", ""},
		{"worktree reads the linked gitdir", "wt", HeadBranch, "feat/live-branch", ""},
		{"rebase in flight names the branch", "rebasing", HeadRebasing, "topic", "fdc9b62"},
		{"rebase from a detached head has none", "rebase-anon", HeadRebasing, "", "aaaaaaa"},
		{"detached is a state, not a name", "detached", HeadDetached, "", "0123456"},
		{"submodule gitdir resolves relative", "super/sub", HeadBranch, "submodule-branch", ""},
		{"symref outside refs/heads is a checkout we cannot name", "symref", HeadUnreadable, "", ""},
		{"unparseable HEAD is a checkout, degraded", "junk", HeadUnreadable, "", ""},
		{"gitdir that isn't there holds no HEAD at all", "dangling", HeadNone, "", ""},
	}
	for _, c := range cases {
		got := repoCache{}.resolve(filepath.Join(root, c.dir))
		if got.Head.Kind != c.kind || got.Head.Branch != c.branch || got.Head.Commit != c.commit {
			t.Errorf("%s: Head = %+v, want {%v %q %q}", c.name, got.Head, c.kind, c.branch, c.commit)
		}
	}

	// Identity must survive the addition: a worktree still keys on its main
	// checkout, a submodule still keys on itself.
	if got := (repoCache{}).resolve(filepath.Join(root, "wt")); got.Key != canon(filepath.Join(root, "main")) {
		t.Errorf("worktree key = %q, want the main checkout", got.Key)
	}
	if got := (repoCache{}).resolve(filepath.Join(root, "super/sub")); got.Key != canon(filepath.Join(root, "super/sub")) {
		t.Errorf("submodule key = %q, want itself", got.Key)
	}
}

// A dir with no repo above it has no HEAD to read and must not acquire one
// from an ancestor that happens to be a checkout of something else.
func TestResolveOutsideARepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := repoCache{}.resolve(filepath.Join(root, "plain"))
	if got.Key != "" || got.Head.Kind != HeadNone {
		t.Errorf("non-repo resolved to %+v", got)
	}
}

// A directory named .git that holds no HEAD is not a checkout. Worktree
// tooling and editors leave these husks behind — the live store has one at
// dev/itell/apps/platform holding nothing but info/exclude — and stopping the
// walk there hides the real repository above it: the session gets keyed on a
// path git does not consider a checkout, and no branch can be read for it.
func TestResolveWalksPastAGitHusk(t *testing.T) {
	root := t.TempDir()
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

	got := repoCache{}.resolve(filepath.Join(root, "repo/apps/platform"))
	if got.Key != canon(filepath.Join(root, "repo")) {
		t.Errorf("Key = %q, want the real checkout above the husk", got.Key)
	}
	if got.Head.Kind != HeadBranch || got.Head.Branch != "feat/summary-attempt-policy" {
		t.Errorf("Head = %+v, want the real checkout's branch", got.Head)
	}

	// With no real repo above it, the husk is still the best evidence there
	// is — the walk must fall back to it, not report "no repo at all".
	write("orphan/.git/info/exclude", "")
	if got := (repoCache{}).resolve(filepath.Join(root, "orphan")); got.Root == "" {
		t.Errorf("a husk with nothing above it resolved to %+v, want it kept as evidence", got)
	}
}

// The two reasons there is no branch to read are not the same fact, and a
// caller that cannot tell them apart cannot render either honestly: no HEAD
// here means this is not a checkout, an unreadable HEAD means it is one and
// something is wrong with it.
func TestHeadDistinguishesAbsentFromUnreadable(t *testing.T) {
	root := t.TempDir()
	mk := func(path, body string) string {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Dir(full)
	}
	absent := mk("no-head/info/exclude", "x")
	unreadable := mk("junk/HEAD", "not a sha and not a ref\n")

	if got := readHead(absent); got.Kind != HeadNone {
		t.Errorf("no HEAD file: Kind = %v, want HeadNone", got.Kind)
	}
	if got := readHead(unreadable); got.Kind != HeadUnreadable {
		t.Errorf("unparseable HEAD: Kind = %v, want HeadUnreadable", got.Kind)
	}
}
