package state

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
)

// place is one ordinary launch: the positional spelling these tests read best in.
func place(s *Store, cands []placement.Candidate, intent placement.Intent, pid int, session string, now time.Time) (Placed, error) {
	return s.Place(Launch{Candidates: cands, Intent: intent, PID: pid, Session: session, Now: now})
}

// rehome records that a session was moved to an account, as a launch that
// places a named session off its owner does.
func rehome(s *Store, id, account string, at time.Time, live Enumerator) error {
	_, err := s.Place(Launch{
		Candidates: idle(account), Intent: placement.Intent{Kind: placement.Forced, Account: account, Reason: "test"},
		Session: id, Live: live, Now: at,
	})
	return err
}

func idle(names ...string) []placement.Candidate {
	out := make([]placement.Candidate, len(names))
	for i, n := range names {
		out[i] = placement.Candidate{Name: n, Ref: n, Key: "uuid:" + n}
	}
	return out
}

// The headline invariant, launches' counterpart of TestClaimIsTestAndSet:
// choosing and recording are one locked operation, so launches that start
// together see each other. Two Store handles in one process contend for the
// real lock exactly as two processes would.
func TestPlaceIsChooseAndRecord(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	cands := idle("a", "b", "c", "d", "e", "f")

	var wg sync.WaitGroup
	got := make([]string, len(cands))
	start := make(chan struct{})
	for i := range cands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := Open(rootScope(root))
			<-start
			p, err := place(s, cands, placement.Intent{}, 1000+i, "", now)
			if err != nil || p.Decision.Chosen == "" {
				t.Errorf("place %d: chose %q err=%v", i, p.Decision.Chosen, err)
			}
			got[i] = p.Decision.Chosen
		}(i)
	}
	close(start)
	wg.Wait()

	sort.Strings(got)
	for i, c := range cands {
		if got[i] != c.Name {
			t.Fatalf("six launches against six equal accounts went to %v", got)
		}
	}
	ledger := Open(rootScope(root)).Load().Placements()
	if len(ledger.Recent) != len(cands) || len(ledger.Last) != len(cands) {
		t.Fatalf("recorded %d recent / %d last, want %d each", len(ledger.Recent), len(ledger.Last), len(cands))
	}
}

// A short job that has already exited still spent what the figures have not
// caught up with: its placement counts until its span has passed, whatever
// became of the process.
func TestAPlacementOutlivesItsProcess(t *testing.T) {
	s := Open(rootScope(t.TempDir()))
	now := time.Now()
	cands := idle("a", "b")
	var got []string
	for i, at := range []time.Duration{0, time.Second, 2 * time.Second} {
		p, err := place(s, cands, placement.Intent{}, 1+i, "", now.Add(at))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p.Decision.Chosen)
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "a" {
		t.Fatalf("placements = %v, want a, b, a", got)
	}
	// Past the span the record no longer counts, and is pruned from the file.
	later := now.Add(placement.PendingFor + time.Minute)
	p, err := place(s, cands, placement.Intent{}, 9, "", later)
	if err != nil || p.Decision.Chosen != "b" {
		// b was placed least recently of the two.
		t.Fatalf("after the span: %q %v", p.Decision.Chosen, err)
	}
	if n := len(s.Load().Placements().Recent); n != 1 {
		t.Fatalf("%d recent placements kept, want the one just made", n)
	}
}

// A candidate offered with no ref is refused rather than recorded: a re-home
// is kept under the ref, and one written under nothing would route to nothing
// without a word.
func TestAChoiceWithNoRefIsNotRecorded(t *testing.T) {
	root := t.TempDir()
	s := Open(rootScope(root))
	cands := []placement.Candidate{{Name: "a", Key: "uuid:a"}}
	if _, err := place(s, cands, placement.Intent{}, 1000, "s1", time.Now()); err == nil {
		t.Fatal("a choice with no ref was placed without a word")
	}
	if n := len(s.Load().Placements().Recent); n != 0 {
		t.Errorf("a choice with no ref was recorded: %d placements", n)
	}
	if _, ok := s.Load().Owners()["s1"]; ok {
		t.Error("a choice with no ref was recorded as a re-home")
	}
}

func TestPlaceForced(t *testing.T) {
	s := Open(rootScope(t.TempDir()))
	now := time.Now()
	cands := idle("a", "b")
	// Named on a, the account the rule would otherwise take first.
	if _, err := place(s, cands, placement.Intent{Kind: placement.Forced, Account: "a", Reason: "named"}, 2, "", now); err != nil {
		t.Fatal(err)
	}
	// A named launch is load for the automatic ones.
	if p, _ := place(s, cands, placement.Intent{}, 3, "", now.Add(time.Second)); p.Decision.Chosen != "b" {
		t.Fatalf("automatic launch after a named one on a chose %q", p.Decision.Chosen)
	}
	// A refused choice records nothing.
	before := s.Load().Placements()
	if p, _ := place(s, cands, placement.Intent{Kind: placement.Forced, Account: "nobody"}, 5, "", now.Add(3*time.Second)); p.Decision.Chosen != "" {
		t.Fatalf("a refusal chose %q", p.Decision.Chosen)
	}
	if after := s.Load().Placements(); len(after.Recent) != len(before.Recent) {
		t.Fatal("a refusal changed the record")
	}
}

// A held lock costs the launch its record and nothing else: it still chooses,
// quickly, from what an unlocked read shows.
func TestPlaceUnderAHeldLockStillChooses(t *testing.T) {
	root := t.TempDir()
	s := Open(rootScope(root))
	now := time.Now()
	cands := idle("a", "b")
	if _, err := place(s, cands, placement.Intent{}, 1, "", now); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(filepath.Join(root, "state.json.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
	before, _ := os.ReadFile(filepath.Join(root, "state.json"))

	start := time.Now()
	p, err := place(s, cands, placement.Intent{}, 2, "", now.Add(time.Second))
	if err != ErrBusy {
		t.Fatalf("err=%v, want ErrBusy", err)
	}
	if p.Decision.Chosen != "b" {
		t.Fatalf("chose %q: the unlocked read still shows the launch on a", p.Decision.Chosen)
	}
	if elapsed := time.Since(start); elapsed > placeWait+time.Second {
		t.Fatalf("blocked for %v", elapsed)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "state.json")); !bytes.Equal(before, after) {
		t.Fatal("the file changed under a lock this call never held")
	}
}

// The placements section is disposable; the sessions section beside it holds
// human decisions. Damage to the first is set aside and rebuilt; the second is
// never written over, and a launch that would have re-homed says it could not.
func TestPlaceDegradesPerSection(t *testing.T) {
	root := t.TempDir()
	doc := `{"version":1,"accounts":{},"placements":"not an object","sessions":{"s1":{"account":"a","atMs":5}}}`
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(rootScope(root))
	if probs := s.Load().Problems(); len(probs) != 1 || probs[0].Section != "placements" {
		t.Fatalf("problems = %+v", probs)
	}
	p, err := place(s, idle("a", "b"), placement.Intent{}, 1, "new-session", time.Now())
	if err != nil || p.Decision.Chosen == "" {
		t.Fatalf("place over an unreadable placements section: %+v %v", p, err)
	}
	raw := readRaw(t, root)
	if string(raw["placements_unreadable"]) != `"not an object"` {
		t.Errorf("unreadable bytes were not set aside: %s", raw["placements_unreadable"])
	}
	snap := s.Load()
	if len(snap.Problems()) != 0 || len(snap.Placements().Recent) != 1 {
		t.Errorf("after the rebuild: problems %v, recent %d", snap.Problems(), len(snap.Placements().Recent))
	}
	if rec, _ := snap.Owners()["s1"]; rec.Account != "a" {
		t.Errorf("an existing re-home was disturbed: %+v", rec)
	}
	if rec, _ := snap.Owners()["new-session"]; rec.Account != "a" {
		t.Errorf("the placed session was not re-homed: %+v", rec)
	}

	// An unreadable sessions section: the placement is recorded, the re-home
	// is refused, and the bytes survive verbatim.
	root = t.TempDir()
	doc = `{"version":1,"accounts":{},"sessions":{"s1":{"account":"","atMs":5}}}`
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s = Open(rootScope(root))
	p, err = place(s, idle("a"), placement.Intent{}, 1, "new-session", time.Now())
	if err != nil || p.Route.Missed() != ErrCorrupt || p.Route.Written() {
		t.Fatalf("place beside an unreadable sessions section: %+v %v", p, err)
	}
	if n := len(s.Load().Placements().Recent); n != 1 {
		t.Errorf("the placement beside an unreadable sessions section was not recorded: %d", n)
	}
	if got := compact(t, readRaw(t, root)["sessions"]); got != `{"s1":{"account":"","atMs":5}}` {
		t.Errorf("sessions section rewritten: %s", got)
	}

	// A document from a newer headroom is read and never written.
	root = t.TempDir()
	doc = `{"version":99,"placements":{"recent":[{"account":"uuid:a","name":"a","pid":7,"at_ms":` +
		itoa(time.Now().UnixMilli()) + `}]}}`
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s = Open(rootScope(root))
	p, err = place(s, idle("a", "b"), placement.Intent{}, 1, "", time.Now())
	if err != ErrReadOnly || p.Decision.Chosen != "b" {
		t.Fatalf("newer schema: %+v %v", p, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "state.json")); string(after) != doc {
		t.Error("a newer document was rewritten")
	}
}

// A launch that follows the session's owner writes no re-home: the routing has
// not changed, and re-stamping it would let intent pass for evidence.
func TestPlaceFollowingTheOwnerWritesNoReHome(t *testing.T) {
	s := Open(rootScope(t.TempDir()))
	now := time.Now()
	p, err := place(s, idle("a", "b"), placement.Intent{Owner: "b"}, 1, "sess", now)
	if err != nil || p.Decision.Chosen != "b" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, ok := s.Load().Owners()["sess"]; ok {
		t.Fatal("following the owner recorded a re-home")
	}
}

// Place says what became of the session's re-home, on its own: written when
// the launch put the session somewhere other than its owner, not called for
// when it followed the owner or was for no session, and — when nothing could
// be recorded at all — missed for that reason exactly where it was owed.
func TestPlaceSaysWhatBecameOfTheReHome(t *testing.T) {
	s := Open(rootScope(t.TempDir()))
	p, err := place(s, idle("a", "b"), placement.Intent{}, 1, "s1", time.Now())
	if err != nil || !p.Route.Written() || p.Route.Missed() != nil {
		t.Fatalf("a session with no owner to follow: %+v, %v", p, err)
	}
	owner := p.Decision.Chosen
	if p, err := place(s, idle(owner), placement.Intent{Owner: owner}, 1, "s1", time.Now()); err != nil || p.Route.Written() || p.Route.Missed() != nil {
		t.Errorf("a session that stayed on its owner: %+v, %v", p, err)
	}
	if p, err := place(s, idle("a", "b"), placement.Intent{}, 1, "", time.Now()); err != nil || p.Route.Written() || p.Route.Missed() != nil {
		t.Errorf("a launch for no session: %+v, %v", p, err)
	}

	root := t.TempDir()
	writeDoc(t, root, `{"version":999}`)
	s = Open(rootScope(root))
	owed := func(session, owner string) Placed {
		p, err := place(s, idle("a", "b"), placement.Intent{Owner: owner}, 1, session, time.Now())
		if err != ErrReadOnly {
			t.Fatalf("err = %v", err)
		}
		return p
	}
	if p := owed("", ""); p.Route.Missed() != nil {
		t.Errorf("nothing recorded, no session: %+v", p)
	}
	if p := owed("s1", ""); p.Route.Missed() != ErrReadOnly {
		t.Errorf("nothing recorded, a session with no owner to follow: %+v", p)
	}
	chosen := owed("", "").Decision.Chosen
	if p := owed("s1", chosen); p.Decision.Chosen != chosen || p.Route.Missed() != nil {
		t.Errorf("nothing recorded, a session that stayed on its owner owed no re-home: %+v", p)
	}
}
