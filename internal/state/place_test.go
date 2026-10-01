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

// rehome moves a session to an account the way the picker's override does: a
// launch that exists to move it.
func rehome(s *Store, id, account string, at time.Time, live Enumerator) error {
	_, err := s.Place(Launch{
		Candidates: idle(account), Intent: placement.Intent{Kind: placement.Forced, Account: account, Reason: "test"},
		Session: id, MustReHome: true, Live: live, Now: at,
	})
	return err
}

func idle(names ...string) []placement.Candidate {
	out := make([]placement.Candidate, len(names))
	for i, n := range names {
		out[i] = placement.Candidate{Name: n, Key: "uuid:" + n}
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
			if err != nil || !p.Recorded {
				t.Errorf("place %d: recorded=%v err=%v", i, p.Recorded, err)
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

func TestPlaceForcedAndLast(t *testing.T) {
	s := Open(rootScope(t.TempDir()))
	now := time.Now()
	cands := idle("a", "b")
	if p, err := place(s, cands, placement.Intent{Kind: placement.LastUsed}, 1, "", now); err != nil || p.Decision.Chosen != "" || p.Recorded {
		t.Fatalf("last with nothing recorded: %+v %v", p, err)
	}
	if _, err := place(s, cands, placement.Intent{Kind: placement.Forced, Account: "b", Reason: "named"}, 2, "", now); err != nil {
		t.Fatal(err)
	}
	// A named launch is load for the automatic ones…
	if p, _ := place(s, cands, placement.Intent{}, 3, "", now.Add(time.Second)); p.Decision.Chosen != "a" {
		t.Fatalf("automatic launch after a named one on b chose %q", p.Decision.Chosen)
	}
	// …and the last account used is whichever launch came last, in any mode.
	if p, _ := place(s, cands, placement.Intent{Kind: placement.LastUsed}, 4, "", now.Add(2*time.Second)); p.Decision.Chosen != "a" {
		t.Fatalf("last = %q, want a", p.Decision.Chosen)
	}
	// A refused choice records nothing.
	before := s.Load().Placements()
	if p, _ := place(s, cands, placement.Intent{Kind: placement.Forced, Account: "nobody"}, 5, "", now.Add(3*time.Second)); p.Recorded {
		t.Fatal("a refusal was recorded")
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
	if err != ErrBusy || p.Recorded {
		t.Fatalf("recorded=%v err=%v, want unrecorded and ErrBusy", p.Recorded, err)
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
	if err != nil || !p.Recorded || !p.ReHomed {
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
	if rec, _ := snap.Owner("s1"); rec.Account != "a" {
		t.Errorf("an existing re-home was disturbed: %+v", rec)
	}
	if rec, _ := snap.Owner("new-session"); rec.Account != "a" {
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
	if err != nil || !p.Recorded || p.ReHomed || p.SessionErr != ErrCorrupt {
		t.Fatalf("place beside an unreadable sessions section: %+v %v", p, err)
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
	if err != ErrReadOnly || p.Recorded || p.Decision.Chosen != "b" {
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
	if err != nil || p.Decision.Chosen != "b" || p.ReHomed {
		t.Fatalf("%+v %v", p, err)
	}
	if _, ok := s.Load().Owner("sess"); ok {
		t.Fatal("following the owner recorded a re-home")
	}
}

// An older binary must carry the section through its own writes: it is not in
// the set this test's "old" writer decodes, exactly as it will not be for a
// binary from before this change.
func TestUnknownSectionsSurvivePlacement(t *testing.T) {
	root := t.TempDir()
	doc := `{"version":1,"accounts":{},"future":{"keep":true}}`
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(rootScope(root))
	if _, err := place(s, idle("a"), placement.Intent{}, 1, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := compact(t, readRaw(t, root)["future"]); got != `{"keep":true}` {
		t.Errorf("unknown section = %s", got)
	}
}
