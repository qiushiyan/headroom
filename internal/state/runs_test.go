package state

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
)

// placeHeadless is a placement a caller starts the session for itself: it
// carries an id and no process yet.
func placeHeadless(t *testing.T, s *Store, id string, cands []placement.Candidate, now time.Time) Placed {
	t.Helper()
	p, err := s.Place(Launch{Candidates: cands, Intent: placement.Intent{CanWait: true}, ID: id, Now: now})
	if err != nil || !p.Recorded {
		t.Fatalf("place %s: recorded=%v err=%v", id, p.Recorded, err)
	}
	return p
}

// A headless placement is recorded under its id with no process, and the
// process reported for it later is given to the placement and recorded as a
// run of the same subscription, once however often it is reported.
func TestAStartedSessionIsAttachedToItsPlacement(t *testing.T) {
	s := Open(rootScope(t.TempDir()))
	now := time.Now()
	p := placeHeadless(t, s, "p1", idle("a", "b"), now)
	rec := s.Load().Placements().Recent
	if len(rec) != 1 || rec[0].ID != "p1" || rec[0].PID != 0 || rec[0].Name != p.Decision.Chosen {
		t.Fatalf("recorded %+v", rec)
	}

	started := now.Add(2 * time.Second).Truncate(time.Second)
	for range 2 {
		run, err := s.Started("p1", 4242, started.UnixMilli(), nil, now.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		want := Run{Placement: "p1", Key: "uuid:" + p.Decision.Chosen, Name: p.Decision.Chosen, PID: 4242, StartedMS: started.UnixMilli(), Home: s.Home()}
		if run != want {
			t.Fatalf("run %+v, want %+v", run, want)
		}
	}
	snap := s.Load()
	if runs := snap.Runs(); len(runs) != 1 || runs[0].PID != 4242 {
		t.Fatalf("runs %+v", runs)
	}
	if rec := snap.Placements().Recent; rec[0].PID != 4242 {
		t.Fatalf("placement not given the process: %+v", rec)
	}

	if _, err := s.Started("nobody", 1, started.UnixMilli(), nil, now); !errors.Is(err, ErrNotPlaced) {
		t.Fatalf("an unknown placement: %v", err)
	}
}

// Runs proven over are dropped by the next write that carries them, and runs
// past their age bound by any; a run recorded after the caller looked is never
// dropped on its say.
func TestRunsAreSweptByEvidenceAndByAge(t *testing.T) {
	s := Open(rootScope(t.TempDir()))
	now := time.Now()
	for _, id := range []string{"old", "done", "live"} {
		placeHeadless(t, s, id, idle("a"), now)
	}
	at := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	if _, err := s.Started("old", 10, at(-runKeep-time.Minute), nil, now); err != nil {
		t.Fatal(err)
	}
	done, err := s.Started("done", 11, at(0), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Started("live", 12, at(0), nil, now); err != nil {
		t.Fatal(err)
	}
	// "old" went with the first write after it; "done" goes with the launch
	// that proved it over.
	placeHeadless(t, s, "next", idle("a"), now.Add(time.Second))
	if _, err := s.Place(Launch{Candidates: idle("a"), Gone: []Run{done}, PID: 1, Now: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	runs := s.Load().Runs()
	if len(runs) != 1 || runs[0].Placement != "live" {
		t.Fatalf("runs kept %+v, want only the live one", runs)
	}
}

// Runs are the subscription's: a second home sharing the ledger records into
// it and reads the first home's.
func TestRunsLiveInTheSharedLedger(t *testing.T) {
	h := twoHomes(t)
	now := time.Now()
	placeHeadless(t, h.second, "p", idle("a"), now)
	if _, err := h.second.Started("p", 99, now.UnixMilli(), nil, now); err != nil {
		t.Fatal(err)
	}
	runs := h.owner.Load().Runs()
	if len(runs) != 1 || runs[0].Home != h.secondRoot {
		t.Fatalf("the owner sees %+v", runs)
	}
	if _, err := os.Stat(statePath(h.secondRoot)); err == nil {
		if b, _ := os.ReadFile(statePath(h.secondRoot)); strings.Contains(string(b), `"runs"`) {
			t.Errorf("a run landed in the second home's own file:\n%s", b)
		}
	}
}

// A runs section that will not decode is reported, read as empty, and set
// aside by the next placement, as the placements section is.
func TestAnUnreadableRunsSectionIsSetAside(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":`+itoa(Version)+`,"runs":[{"placement":"x","account":"uuid:a","pid":0}]}`)
	s := Open(rootScope(root))
	snap := s.Load()
	if len(snap.Runs()) != 0 || !hasProblem(snap.Problems(), "runs") {
		t.Fatalf("runs %+v problems %+v", snap.Runs(), snap.Problems())
	}
	placeHeadless(t, s, "p", idle("a"), time.Now())
	b, _ := os.ReadFile(statePath(root))
	if !strings.Contains(string(b), `"runs_unreadable"`) || hasProblem(s.Load().Problems(), "runs") {
		t.Fatalf("not set aside:\n%s", b)
	}
}

func hasProblem(ps []Problem, section string) bool {
	for _, p := range ps {
		if p.Section == section {
			return true
		}
	}
	return false
}
