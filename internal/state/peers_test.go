package state

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

var body = json.RawMessage(`{"limits":[]}`)

// Of the surfaces of one machine asking at once, one opens the connection:
// the claim is a test-and-set like the request claim, and the next pull is
// due a spacing later.
func TestOnlyOneSurfacePullsAtOnce(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	const racers = 8
	var wg sync.WaitGroup
	permits := make([]bool, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			s := Open(rootScope(root))
			<-start
			permits[i], _ = s.ClaimPull("mini2", now)
		})
	}
	close(start)
	wg.Wait()
	granted := 0
	for _, ok := range permits {
		if ok {
			granted++
		}
	}
	if granted != 1 {
		t.Errorf("%d of %d racers may pull; exactly one opens the connection", granted, racers)
	}

	s := Open(rootScope(root))
	spacing := rootScope(root).RequestSpacing()
	if ok, _ := s.ClaimPull("mini2", now.Add(spacing-time.Second)); ok {
		t.Error("a pull inside the spacing was permitted")
	}
	if ok, _ := s.ClaimPull("mac", now); !ok {
		t.Error("another machine's pull waited on this one's")
	}
	if ok, _ := s.ClaimPull("mini2", now.Add(spacing)); !ok {
		t.Error("a pull a spacing later was refused")
	}
}

// A machine that did not answer is left alone for longer than a spacing, and
// its last answer stands: a failure annotates what is known and never erases
// it.
func TestAFailedPullKeepsTheLastAnswer(t *testing.T) {
	root := t.TempDir()
	s := Open(rootScope(root))
	now := time.Now()
	k := Key{UUID: "a1", Name: "a@example.com"}

	s.ClaimPull("mini2", now)
	if err := s.RecordPull("mini2", map[string]Observation{k.ID(): {FetchedAtMS: now.UnixMilli(), Body: body}}, "", now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(rootScope(root).RequestSpacing())
	if ok, _ := s.ClaimPull("mini2", later); !ok {
		t.Fatal("the next pull was refused")
	}
	if err := s.RecordPull("mini2", nil, "ssh could not reach it", later); err != nil {
		t.Fatal(err)
	}

	snap := s.Load()
	if _, ok := snap.PeerObservation("mini2", k, later); !ok {
		t.Error("a failed pull erased the last answer")
	}
	st, _ := snap.PeerStatus("mini2")
	if st.Error != "ssh could not reach it" || st.PulledMS != now.UnixMilli() {
		t.Errorf("status = %+v, want the failure beside the last answer's time", st)
	}
	if ok, _ := s.ClaimPull("mini2", later.Add(rootScope(root).RequestSpacing())); ok {
		t.Error("a machine that did not answer was asked again a spacing later")
	}
	if ok, _ := s.ClaimPull("mini2", later.Add(PeerRetry)); !ok {
		t.Error("a machine that did not answer was never asked again")
	}
}

// An answer replaces the machine's last one whole, and only readings keyed by
// identity are kept: another machine's dir name says nothing about which
// account is in it. A reading is matched to an account by identity alone.
func TestAnAnswerIsKeptByIdentity(t *testing.T) {
	root := t.TempDir()
	s := Open(rootScope(root))
	now := time.Now()
	a, b := Key{UUID: "a1", Name: "a@example.com"}, Key{UUID: "b1", Name: "b@example.com"}

	s.ClaimPull("mini2", now)
	s.RecordPull("mini2", map[string]Observation{
		a.ID():               {FetchedAtMS: now.UnixMilli(), Body: body},
		b.ID():               {FetchedAtMS: now.UnixMilli(), Body: body},
		"dir:c@example.com":  {FetchedAtMS: now.UnixMilli(), Body: body},
		"uuid:empty":         {FetchedAtMS: now.UnixMilli()},
		"uuid:never-fetched": {Body: body},
	}, "", now)
	got := s.Load().d.peers["mini2"].Readings
	if len(got) != 2 {
		t.Errorf("kept %v, want the two readings keyed by identity", got)
	}

	s.RecordPull("mini2", map[string]Observation{a.ID(): {FetchedAtMS: now.UnixMilli(), Body: body}}, "", now)
	snap := s.Load()
	if _, ok := snap.PeerObservation("mini2", b, now); ok {
		t.Error("an account the newest answer left out still has the old answer's reading")
	}
	if _, ok := snap.PeerObservation("mini2", Key{Name: "a@example.com"}, now); ok {
		t.Error("a reading was matched to an account known only by its dir's name")
	}
	if _, ok := snap.PeerObservation("mac", a, now); ok {
		t.Error("one machine's reading was given as another's")
	}
}

// A reading stamped in the future is a clock anomaly, refused as the store's
// own is; and an ask stamped in the future does not hold pulls off.
func TestPeerClockAnomaliesAreNotTrusted(t *testing.T) {
	root := t.TempDir()
	s := Open(rootScope(root))
	now := time.Now()
	k := Key{UUID: "a1"}
	ahead := now.Add(time.Hour)

	s.ClaimPull("mini2", ahead)
	s.RecordPull("mini2", map[string]Observation{k.ID(): {FetchedAtMS: ahead.UnixMilli(), Body: body}}, "", ahead)
	if _, ok := s.Load().PeerObservation("mini2", k, now); ok {
		t.Error("a reading stamped an hour ahead was taken as data")
	}
	if ok, _ := s.ClaimPull("mini2", now); !ok {
		t.Error("an ask stamped in the future held the next pull off")
	}
}

// The peers section is disposable: one that will not decode is reported,
// reads as empty, and is set aside at the next pull — never written over
// unread.
func TestAnUnreadablePeersSectionIsSetAside(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"peers":{"mini2":"not a record"}}`)
	s := Open(rootScope(root))
	snap := s.Load()
	if len(snap.Problems()) != 1 || snap.Problems()[0].Section != "peers" {
		t.Errorf("problems = %v, want the peers section named", snap.Problems())
	}
	if ok, err := s.ClaimPull("mini2", time.Now()); !ok || err != nil {
		t.Fatalf("pull refused over an unreadable section: %v %v", ok, err)
	}
	raw := readRaw(t, root)
	if _, ok := raw["peers_unreadable"]; !ok {
		t.Errorf("the unreadable section was not kept aside: %v", raw)
	}
	if len(s.Load().Problems()) != 0 {
		t.Errorf("the rebuilt section still reads as unreadable: %v", s.Load().Problems())
	}
}

// A machine nobody has asked in the ledger's retention is dropped, by age
// alone.
func TestAMachineNobodyAsksAgesOut(t *testing.T) {
	root := t.TempDir()
	s := Open(rootScope(root))
	then := time.Now().Add(-retention - time.Hour)
	s.ClaimPull("old", then)
	s.ClaimPull("mini2", time.Now())
	if _, ok := s.Load().PeerStatus("old"); ok {
		t.Error("a machine asked a retention ago is still recorded")
	}
	if _, ok := s.Load().PeerStatus("mini2"); !ok {
		t.Error("the machine just asked is not recorded")
	}
}
