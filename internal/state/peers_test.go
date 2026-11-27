package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
)

var body = json.RawMessage(`{"limits":[]}`)

// pairedScope is rootScope naming one peer, so its store reads the peers
// section.
func pairedScope(root string) config.Scope {
	s := rootScope(root)
	s.Peers = []string{"mini2"}
	return s
}

// pull claims a pull from peer at at and records readings as its answer.
func pull(t *testing.T, s *Store, peer string, at time.Time, readings map[string]Observation) {
	t.Helper()
	c, err := s.ClaimPull(peer, at)
	if err != nil || !c.Permit {
		t.Fatalf("pull from %s at %v refused: %+v %v", peer, at, c, err)
	}
	if err := s.RecordPull(peer, c.Generation, readings, "", at); err != nil {
		t.Fatal(err)
	}
}

func reading(at time.Time) Observation { return Observation{FetchedAtMS: at.UnixMilli(), Body: body} }

// Of the surfaces of one machine asking at once, one opens the connection:
// the claim is a test-and-set like the request claim, and the next pull is
// due a spacing later — the instant NextPull names.
func TestOnlyOneSurfacePullsAtOnce(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	const racers = 8
	var wg sync.WaitGroup
	permits := make([]bool, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			s := Open(pairedScope(root))
			<-start
			c, _ := s.ClaimPull("mini2", now)
			permits[i] = c.Permit
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

	s := Open(pairedScope(root))
	spacing := rootScope(root).RequestSpacing()
	if next := s.Load().NextPull("mini2", now); !next.Equal(time.UnixMilli(now.Add(spacing).UnixMilli())) {
		t.Errorf("next pull at %v, want a spacing after the ask", next)
	}
	if c, _ := s.ClaimPull("mini2", now.Add(spacing-time.Second)); c.Permit {
		t.Error("a pull inside the spacing was permitted")
	}
	if c, _ := s.ClaimPull("mac", now); !c.Permit {
		t.Error("another machine's pull waited on this one's")
	}
	if c, _ := s.ClaimPull("mini2", now.Add(spacing)); !c.Permit {
		t.Error("a pull a spacing later was refused")
	}
}

// A machine that did not answer is left alone for longer than a spacing; its
// readings stand, and the failure keeps its own time — a later ask that was
// cut short does not take it over.
func TestAFailedPullKeepsTheLastAnswer(t *testing.T) {
	root := t.TempDir()
	s := Open(pairedScope(root))
	now := time.Now()
	k := Key{UUID: "a1"}
	spacing := rootScope(root).RequestSpacing()

	pull(t, s, "mini2", now, map[string]Observation{k.ID(): reading(now)})
	later := now.Add(spacing)
	c, _ := s.ClaimPull("mini2", later)
	if err := s.RecordPull("mini2", c.Generation, nil, "ssh could not reach it", later); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load().PeerObservation("mini2", k, later); !ok {
		t.Error("a failed pull erased the last answer")
	}
	if c, _ := s.ClaimPull("mini2", later.Add(spacing)); c.Permit {
		t.Error("a machine that did not answer was asked again a spacing later")
	}
	if next := s.Load().NextPull("mini2", later); !next.Equal(time.UnixMilli(later.Add(PeerRetry).UnixMilli())) {
		t.Errorf("next pull at %v, want PeerRetry after the failed ask", next)
	}
	retry := later.Add(PeerRetry)
	if c, _ := s.ClaimPull("mini2", retry); !c.Permit {
		t.Fatal("a machine that did not answer was never asked again")
	}
	st, _ := s.Load().PeerStatus("mini2")
	if st.Error != "ssh could not reach it" || st.FailedMS != later.UnixMilli() || st.PulledMS != now.UnixMilli() || st.AttemptMS != retry.UnixMilli() || !st.Pending {
		t.Errorf("status = %+v, want each outcome under its own time and the new ask pending", st)
	}
}

// Which outcome came last never rests on a clock: an answer after a failure
// clears it even when the clock has stepped back past the failure's time,
// and the next pull is due a spacing on, not a retry on.
func TestAnAnswerClearsTheFailureWhateverTheClock(t *testing.T) {
	s := Open(pairedScope(t.TempDir()))
	now := time.Now()
	c, _ := s.ClaimPull("mini2", now)
	s.RecordPull("mini2", c.Generation, nil, "ssh could not reach it", now)
	// The clock stepped back an hour: the last ask now reads as stamped in
	// the future, so the pull is due, and its answer lands before the
	// failure's time.
	back := now.Add(-time.Hour)
	if c, _ = s.ClaimPull("mini2", back); !c.Permit {
		t.Fatal("an ask stamped in the future held the pull off")
	}
	s.RecordPull("mini2", c.Generation, map[string]Observation{"uuid:a1": reading(back)}, "", back)
	st, _ := s.Load().PeerStatus("mini2")
	if st.Error != "" || st.FailedMS != 0 || st.Pending {
		t.Errorf("status = %+v, want the answer to stand as the newest outcome", st)
	}
}

// A reading a reader would refuse never enters the record: one stamped too
// far ahead would otherwise stand over every later, valid reading of the
// account, which the reader would then never see. A reading past the
// ledger's retention is not read.
func TestOnlyUsableReadingsAreKeptAndRead(t *testing.T) {
	s := Open(pairedScope(t.TempDir()))
	now := time.Now()
	k := Key{UUID: "a1"}
	pull(t, s, "mini2", now, map[string]Observation{k.ID(): reading(now.Add(time.Hour))})
	pull(t, s, "mini2", now.Add(rootScope("").RequestSpacing()), map[string]Observation{k.ID(): reading(now)})
	if o, ok := s.Load().PeerObservation("mini2", k, now); !ok || o.FetchedAtMS != now.UnixMilli() {
		t.Errorf("a reading stamped an hour ahead hid the valid one: %+v %v", o, ok)
	}
	if _, ok := s.Load().PeerObservation("mini2", k, now.Add(retention+time.Hour)); ok {
		t.Error("a reading past the ledger's retention was read")
	}
}

// A document no claim can write says the next pull is not soon, so neither a
// launch nor the board spins on a claim that must fail.
func TestNoPullIsDueOnADocumentNoClaimCanWrite(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":99}`)
	now := time.Now()
	if next := Open(pairedScope(root)).Load().NextPull("mini2", now); !next.After(now.Add(rootScope(root).RequestSpacing())) {
		t.Errorf("next pull %v on a newer headroom's document", next)
	}
}

// An outcome that outlived its claim is dropped: a late answer cannot stand
// over a newer one, nor a late failure put a machine that has since answered
// back into retry.
func TestALateCompletionIsDropped(t *testing.T) {
	root := t.TempDir()
	s := Open(pairedScope(root))
	now := time.Now()
	k := Key{UUID: "a1"}
	slow, _ := s.ClaimPull("mini2", now)
	later := now.Add(2 * rootScope(root).RequestSpacing())
	pull(t, s, "mini2", later, map[string]Observation{k.ID(): {FetchedAtMS: later.UnixMilli(), Body: json.RawMessage(`{"limits":[],"n":2}`)}})

	s.RecordPull("mini2", slow.Generation, map[string]Observation{"uuid:b1": reading(now)}, "", later.Add(time.Second))
	s.RecordPull("mini2", slow.Generation, nil, "no answer within 8s", later.Add(time.Second))
	snap := s.Load()
	if o, ok := snap.PeerObservation("mini2", k, later); !ok || o.FetchedAtMS != later.UnixMilli() {
		t.Errorf("a late completion touched the newer answer: %+v", o)
	}
	if _, ok := snap.PeerObservation("mini2", Key{UUID: "b1"}, later); ok {
		t.Error("a late answer's readings were kept")
	}
	if st, _ := snap.PeerStatus("mini2"); st.Error != "" || st.FailedMS != 0 {
		t.Errorf("a late failure was recorded over the newer answer: %+v", st)
	}
}

// An answer renews the readings it carries and takes none away: one that
// leaves an account out — a machine whose own ledger could not be read, a
// dir whose identity file was mid-rewrite — says nothing about the dated
// reading held. Only readings keyed by identity, with a body the store keeps,
// are kept; and an older reading never replaces a newer one.
func TestAnAnswerRenewsAndNeverErases(t *testing.T) {
	root := t.TempDir()
	s := Open(pairedScope(root))
	now := time.Now()
	a, b := Key{UUID: "a1"}, Key{UUID: "b1"}
	big := json.RawMessage(`{"limits":[],"pad":"` + string(bytes.Repeat([]byte("x"), BodyLimit)) + `"}`)

	pull(t, s, "mini2", now, map[string]Observation{
		a.ID():               reading(now),
		b.ID():               reading(now),
		"dir:c@example.com":  reading(now),
		"uuid:empty":         {FetchedAtMS: now.UnixMilli()},
		"uuid:never-fetched": {Body: body},
		"uuid:big":           {FetchedAtMS: now.UnixMilli(), Body: big},
	})
	if got := s.Load().d.peers["mini2"].Readings; len(got) != 2 {
		t.Errorf("kept %d readings, want the two keyed by identity with a body the store keeps", len(got))
	}

	later := now.Add(rootScope(root).RequestSpacing())
	pull(t, s, "mini2", later, map[string]Observation{a.ID(): {FetchedAtMS: now.Add(-time.Hour).UnixMilli(), Body: body}})
	snap := s.Load()
	if o, ok := snap.PeerObservation("mini2", a, later); !ok || o.FetchedAtMS != now.UnixMilli() {
		t.Errorf("an older reading replaced a newer one: %+v", o)
	}
	if _, ok := snap.PeerObservation("mini2", b, later); !ok {
		t.Error("an answer that left an account out erased its reading")
	}
	if _, ok := snap.PeerObservation("mini2", Key{Name: "a@example.com"}, later); ok {
		t.Error("a reading was matched to an account known only by its dir's name")
	}
	if _, ok := snap.PeerObservation("mac", a, later); ok {
		t.Error("one machine's reading was given as another's")
	}
}

// One machine's readings are bounded: past peerReadingsMax the oldest go.
func TestOneMachinesReadingsAreBounded(t *testing.T) {
	s := Open(pairedScope(t.TempDir()))
	now := time.Now()
	many := map[string]Observation{}
	for i := range peerReadingsMax + 10 {
		many[fmt.Sprintf("uuid:%d", i)] = reading(now.Add(-time.Duration(i) * time.Second))
	}
	pull(t, s, "mini2", now, many)
	if st, _ := s.Load().PeerStatus("mini2"); st.Readings != peerReadingsMax {
		t.Errorf("kept %d readings, want %d", st.Readings, peerReadingsMax)
	}
	if _, ok := s.Load().PeerObservation("mini2", Key{UUID: "0"}, now); !ok {
		t.Error("the newest reading was the one dropped")
	}
}

// A reading stamped in the future is a clock anomaly, refused as the store's
// own is; and an ask stamped in the future does not hold pulls off.
func TestPeerClockAnomaliesAreNotTrusted(t *testing.T) {
	s := Open(pairedScope(t.TempDir()))
	now := time.Now()
	k := Key{UUID: "a1"}
	ahead := now.Add(time.Hour)

	pull(t, s, "mini2", ahead, map[string]Observation{k.ID(): reading(ahead)})
	if _, ok := s.Load().PeerObservation("mini2", k, now); ok {
		t.Error("a reading stamped an hour ahead was taken as data")
	}
	if c, _ := s.ClaimPull("mini2", now); !c.Permit {
		t.Error("an ask stamped in the future held the next pull off")
	}
}

// The peers section is disposable: one that will not decode is reported,
// reads as empty, and is set aside at the next pull — never written over
// unread.
func TestAnUnreadablePeersSectionIsSetAside(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"peers":{"mini2":"not a record"}}`)
	s := Open(pairedScope(root))
	snap := s.Load()
	if len(snap.Problems()) != 1 || snap.Problems()[0].Section != "peers" {
		t.Errorf("problems = %v, want the peers section named", snap.Problems())
	}
	if c, err := s.ClaimPull("mini2", time.Now()); !c.Permit || err != nil {
		t.Fatalf("pull refused over an unreadable section: %+v %v", c, err)
	}
	if _, ok := readRaw(t, root)["peers_unreadable"]; !ok {
		t.Error("the unreadable section was not kept aside")
	}
	if len(s.Load().Problems()) != 0 {
		t.Errorf("the rebuilt section still reads as unreadable: %v", s.Load().Problems())
	}
}

// A home that names no peer reads as one that never knew of peers: its
// leftover section — damaged or not — is not read, not reported, carried
// through every write as it stands, and pulls from nobody.
func TestAHomeNamingNoPeerLeavesTheSectionAlone(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"peers":{"mini2":"not a record"}}`)
	s := Open(rootScope(root))
	if p := s.Load().Problems(); len(p) != 0 {
		t.Errorf("a home naming no peer reported its leftover section: %v", p)
	}
	if c, _ := s.ClaimPull("mini2", time.Now()); c.Permit {
		t.Error("a home naming no peer was permitted a pull")
	}
	claimOne(t, s, key("a"), time.Now())
	if got := string(readRaw(t, root)["peers"]); got != `{"mini2":"not a record"}` && got != `{
    "mini2": "not a record"
  }` {
		t.Errorf("the leftover section was rewritten: %s", got)
	}
}

// A file that was not JSON owes the ledger a quiet rebuild at the first
// claim, whichever write comes first: a pull landing before any claim must
// not leave a document whose ledger reads as merely empty.
func TestAPullBeforeTheFirstClaimKeepsTheLedgerQuiet(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `not json`)
	s := Open(pairedScope(root))
	if c, err := s.ClaimPull("mini2", time.Now()); !c.Permit || err != nil {
		t.Fatalf("pull refused: %+v %v", c, err)
	}
	if dec := claimOne(t, s, Key{UUID: "a1"}, time.Now()); dec.Permit {
		t.Error("a pull's write ended the quiet period an unreadable ledger owes")
	}
}

// A machine nobody has asked in the ledger's retention is dropped, by age
// alone.
func TestAMachineNobodyAsksAgesOut(t *testing.T) {
	s := Open(pairedScope(t.TempDir()))
	s.ClaimPull("old", time.Now().Add(-retention-time.Hour))
	s.ClaimPull("mini2", time.Now())
	if _, ok := s.Load().PeerStatus("old"); ok {
		t.Error("a machine asked a retention ago is still recorded")
	}
	if _, ok := s.Load().PeerStatus("mini2"); !ok {
		t.Error("the machine just asked is not recorded")
	}
}
