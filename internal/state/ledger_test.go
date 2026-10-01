package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/placement"
)

// homes is two homes on one machine: the owner's, whose accounts root holds
// the ledger, and a second home that spends against it and keeps its own
// re-homes.
type homes struct {
	ownerRoot, secondRoot string
	owner, second         *Store
}

func twoHomes(t *testing.T) homes {
	t.Helper()
	ownerHome, secondHome := t.TempDir(), t.TempDir()
	ownerScope := config.ForHome(ownerHome).Claude
	secondScope := config.ForHome(secondHome).Claude
	secondScope.PrimaryExplicit = true
	secondScope.LedgerRoot = ownerScope.AccountsRoot
	for _, s := range []config.Scope{ownerScope, secondScope} {
		if err := os.MkdirAll(s.AccountsRoot, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return homes{ownerScope.AccountsRoot, secondScope.AccountsRoot, Open(ownerScope), Open(secondScope)}
}

// The budget is the subscription's. A claim one home makes is the claim the
// other is denied by, and what one home bought is what the other replays.
func TestTwoHomesSpendOneRequestLedger(t *testing.T) {
	h := twoHomes(t)
	now := time.Now()
	k := Key{UUID: "u-shared", Name: "yan@planlab.ai"}

	dec := claimOne(t, h.second, k, now)
	if !dec.Permit {
		t.Fatal("the first claim was denied")
	}
	if claimOne(t, h.owner, k, now.Add(time.Second)).Permit {
		t.Fatal("the owner's home asked about a subscription the second home had just claimed")
	}
	if _, err := h.second.Complete(k, dec.Generation, OutcomeStored, []byte(`{"limits":[]}`), now); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.owner.Load().Observation(k, now); !ok {
		t.Error("the owner's home does not see the response the second home stored")
	}
	if _, err := os.Stat(filepath.Join(h.secondRoot, "state.json")); err == nil {
		t.Error("the second home wrote a request ledger of its own")
	}
	if next := h.second.Load().NextEligible(k, now); !next.After(now) {
		t.Error("the second home reads no quiet period from the ledger it spends against")
	}
}

// A launch's load goes to the ledger, where every home counts it; what it
// means for one home's session stays in that home's own file, where no other
// home lists, routes or sweeps it.
func TestALaunchsLoadIsSharedAndItsReHomeIsNot(t *testing.T) {
	h := twoHomes(t)
	now := time.Now()
	const sid = "aaaaaaaa-1111-4111-8111-111111111111"

	p, err := place(h.second, idle("a", "b"), placement.Intent{}, 42, sid, now)
	if err != nil || !p.Recorded || !p.ReHomed {
		t.Fatalf("second home's launch: %+v %v", p, err)
	}
	led := h.owner.Load().Placements()
	if len(led.Recent) != 1 || led.Recent[0].Home != h.secondRoot || led.Recent[0].PID != 42 {
		t.Fatalf("the owner's view of recent launches = %+v", led.Recent)
	}
	if _, ok := h.owner.Load().Owner(sid); ok {
		t.Error("the second home's re-home is visible as the owner's")
	}
	if rec, ok := h.second.Load().Owner(sid); !ok || rec.Account != p.Decision.Chosen {
		t.Errorf("the second home's re-home = %+v %v", rec, ok)
	}

	// The next launch in the owner's home counts it: the launch went to the
	// subscription the second home just placed on.
	q, err := place(h.owner, idle(p.Decision.Chosen, "c"), placement.Intent{}, 43, "", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if q.Decision.Chosen != "c" {
		t.Errorf("the owner's launch went to %s, beside the second home's pending launch", q.Decision.Chosen)
	}
	if c, _ := q.Decision.Find(p.Decision.Chosen); c.Pending != 1 || len(c.Elsewhere(h.ownerRoot)) != 1 {
		t.Errorf("counted = %+v", c)
	}

	// --last is per home.
	if d := placement.Choose(idle("a", "b", "c"), h.second.Load().Placements(), placement.Intent{Kind: placement.LastUsed, Home: h.secondRoot}, now); d.Chosen != p.Decision.Chosen {
		t.Errorf("the second home's last = %q, want its own %q", d.Chosen, p.Decision.Chosen)
	}
	if d := placement.Choose(idle("a", "b", "c"), h.owner.Load().Placements(), placement.Intent{Kind: placement.LastUsed, Home: h.ownerRoot}, now); d.Chosen != "c" {
		t.Errorf("the owner's last = %q, want c", d.Chosen)
	}
}

// Each home's sweep sees its own transcripts and touches its own re-homes.
// The owner's re-homes sit in the very file the second home writes its
// launches to, and still survive a sweep that would have taken them.
func TestEachHomeSweepsOnlyItsOwnReHomes(t *testing.T) {
	h := twoHomes(t)
	old := time.Now().Add(-time.Hour)
	const (
		ownerSession  = "11111111-1111-4111-8111-111111111111"
		secondSession = "22222222-2222-4222-8222-222222222222"
		fresh         = "33333333-3333-4333-8333-333333333333"
	)
	if err := rehome(h.owner, ownerSession, "a", old, nil); err != nil {
		t.Fatal(err)
	}
	if err := rehome(h.second, secondSession, "a", old, nil); err != nil {
		t.Fatal(err)
	}
	none := func() (map[string]bool, bool) { return map[string]bool{}, true }

	// The second home's launch sweeps against its own (empty) store.
	if _, err := h.second.Place(Launch{Candidates: idle("a"), Session: fresh, Live: none, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.owner.Load().Owner(ownerSession); !ok {
		t.Fatal("the second home's sweep took the owner's re-home")
	}
	if _, ok := h.second.Load().Owner(secondSession); ok {
		t.Error("the second home's own orphan survived its sweep")
	}

	// And the reverse.
	if err := rehome(h.second, secondSession, "a", old, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.owner.Place(Launch{Candidates: idle("a"), Session: fresh, Live: none, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.second.Load().Owner(secondSession); !ok {
		t.Error("the owner's sweep took the second home's re-home")
	}
	if _, ok := h.owner.Load().Owner(ownerSession); ok {
		t.Error("the owner's own orphan survived its sweep")
	}
}

// A move is refused when its re-home cannot be written, and a refused move
// leaves nothing in the shared ledger either.
func TestARefusedMoveLeavesTheLedgerUntouched(t *testing.T) {
	h := twoHomes(t)
	write(t, filepath.Join(h.secondRoot, "state.json"), `{"version":1,"sessions":{"x":{}}}`)
	_, err := h.second.Place(Launch{
		Candidates: idle("a"), Intent: placement.Intent{Kind: placement.Forced, Account: "a", Reason: "test"},
		Session: "44444444-4444-4444-8444-444444444444", MustReHome: true, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("a move whose re-home cannot be written was not refused")
	}
	if led := h.owner.Load().Placements(); len(led.Recent) != 0 {
		t.Errorf("a refused move left a placement: %+v", led.Recent)
	}

	// An ordinary launch from that home still records its load, and says
	// its routing was not written.
	p, err := place(h.second, idle("a"), placement.Intent{}, 7, "55555555-5555-4555-8555-555555555555", time.Now())
	if err != nil || !p.Recorded || p.ReHomed || p.SessionErr == nil {
		t.Errorf("ordinary launch: %+v %v", p, err)
	}
}

// Every home that spends against a ledger is registered in it, so the others
// can find its account dirs; a claim that permits nothing writes nothing.
func TestHomesRegisterInTheLedger(t *testing.T) {
	h := twoHomes(t)
	now := time.Now()
	claimOne(t, h.owner, key("a"), now)
	if err := h.second.Register(now); err != nil {
		t.Fatal(err)
	}
	members := h.owner.Load().Members()
	if len(members) != 2 {
		t.Fatalf("members = %+v", members)
	}
	byRoot := map[string]Member{}
	for _, m := range members {
		byRoot[m.Root] = m
	}
	if m := byRoot[h.secondRoot]; !m.Explicit || filepath.Join(m.Home, ".claude-accounts") != h.secondRoot {
		t.Errorf("second home's registration = %+v", m)
	}
	if m := byRoot[h.ownerRoot]; m.Explicit {
		t.Errorf("owner's registration = %+v", m)
	}

	path := filepath.Join(h.ownerRoot, "state.json")
	before, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	claimOne(t, h.second, key("a"), now.Add(time.Second))
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("a denied claim rewrote the ledger to re-register a current home")
	}

	// A home nobody has seen for the retention period drops out.
	if _, err := h.owner.Claim([]Key{key("z")}, now.Add(retention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := h.owner.Load().Members(); len(got) != 1 || got[0].Root != h.ownerRoot {
		t.Errorf("after retention: %+v", got)
	}
}

// Launches recorded before homes shared a ledger were the ledger's own home's:
// they read that way, and never as another home's last account.
func TestLaunchesFromBeforeHomesBelongToTheLedgersHome(t *testing.T) {
	h := twoHomes(t)
	at := time.Now().Add(-time.Minute).UnixMilli()
	write(t, filepath.Join(h.ownerRoot, "state.json"), `{"version":1,"placements":{
		"recent":[{"account":"uuid:a","name":"a","pid":9,"at_ms":`+itoa(at)+`}],
		"last":{"uuid:a":{"name":"a","at_ms":`+itoa(at)+`}}}}`)
	for _, s := range []*Store{h.owner, h.second} {
		led := s.Load().Placements()
		if len(led.Recent) != 1 || led.Recent[0].Home != h.ownerRoot {
			t.Errorf("recent = %+v", led.Recent)
		}
		if last, ok := led.Newest(h.ownerRoot); !ok || last.Name != "a" {
			t.Errorf("the owner's last = %+v %v", last, ok)
		}
		if _, ok := led.Newest(h.secondRoot); ok {
			t.Error("a launch from before homes reads as the second home's last")
		}
	}
}

// Placements from both homes, started together, all land: the ledger's lock
// is the one every launch takes first, whichever home it comes from.
func TestLaunchesFromTwoHomesAllLand(t *testing.T) {
	h := twoHomes(t)
	now := time.Now()
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := h.owner
			sid := ""
			if i%2 == 1 {
				s = h.second
				sid = "66666666-6666-4666-8666-6666666666" + itoa(int64(10+i))
			}
			if p, err := place(s, idle("a", "b", "c"), placement.Intent{}, 100+i, sid, now); err != nil || !p.Recorded {
				t.Errorf("launch %d: %+v %v", i, p.Recorded, err)
			}
		}(i)
	}
	wg.Wait()
	led := h.owner.Load().Placements()
	if len(led.Recent) != 12 {
		t.Fatalf("%d of 12 launches recorded", len(led.Recent))
	}
	perAccount := map[string]int{}
	for _, p := range led.Recent {
		perAccount[p.Name]++
	}
	if perAccount["a"] != 4 || perAccount["b"] != 4 || perAccount["c"] != 4 {
		t.Errorf("twelve launches across two homes spread as %v, want four each", perAccount)
	}
	if n := len(h.second.Load().Owners()); n != 6 {
		t.Errorf("%d of the second home's six sessions were re-homed", n)
	}
	var raw map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(h.secondRoot, "state.json"))
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["placements"]; ok {
		t.Error("the second home's own file holds placements")
	}
}

// When the ledger and the home are two files, the two writes are ordered so
// that a refusal leaves nothing, and each half-written outcome is reported as
// what it is: the load without the routing, or the routing without the load.
func TestAPairWhoseSecondWriteFailsSaysWhichHalfLanded(t *testing.T) {
	readOnly := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
	}
	const sid = "abababab-1111-4111-8111-111111111111"

	t.Run("an ordinary launch whose re-home cannot be written", func(t *testing.T) {
		h := twoHomes(t)
		if err := h.second.Register(time.Now()); err != nil { // creates both lock files
			t.Fatal(err)
		}
		if err := h.second.Forget("nothing"); err != nil {
			t.Fatal(err)
		}
		readOnly(t, h.secondRoot)
		p, err := place(h.second, idle("a"), placement.Intent{}, 9, sid, time.Now())
		if err != nil || !p.Recorded || p.ReHomed || p.SessionErr == nil || p.RecordErr != nil {
			t.Fatalf("placed %+v, %v — want the load recorded and the routing reported missing", p, err)
		}
		if len(h.owner.Load().Placements().Recent) != 1 {
			t.Error("the load did not land in the ledger")
		}
		if _, ok := h.second.Load().Owner(sid); ok {
			t.Error("a re-home reported missing is on disk")
		}
	})

	t.Run("a move whose load cannot be written", func(t *testing.T) {
		h := twoHomes(t)
		if err := h.second.Register(time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := h.second.Forget("nothing"); err != nil {
			t.Fatal(err)
		}
		readOnly(t, h.ownerRoot)
		p, err := h.second.Place(Launch{
			Candidates: idle("a"), Intent: placement.Intent{Kind: placement.Forced, Account: "a", Reason: "test"},
			Session: sid, MustReHome: true, Now: time.Now(),
		})
		if err != nil || p.Recorded || !p.ReHomed || p.RecordErr == nil {
			t.Fatalf("placed %+v, %v — want the move routed and its load reported missing", p, err)
		}
		if rec, ok := h.second.Load().Owner(sid); !ok || rec.Account != "a" {
			t.Error("the re-home the move depended on is not on disk")
		}
		if len(h.owner.Load().Placements().Recent) != 0 {
			t.Error("a load reported missing is in the ledger")
		}
	})
}

// A registration follows its home: a changed home dir or primary selection is
// written at once, a current one is renewed daily so it does not age out
// while the home is in use, and a section that will not decode is set aside
// and rebuilt by the next request.
func TestARegistrationIsMaintained(t *testing.T) {
	h := twoHomes(t)
	now := time.Now()
	member := func() Member {
		t.Helper()
		for _, m := range h.owner.Load().Members() {
			if m.Root == h.secondRoot {
				return m
			}
		}
		t.Fatal("the second home is not registered")
		return Member{}
	}
	if err := h.second.Register(now); err != nil {
		t.Fatal(err)
	}
	first := member()

	// Its primary selection changes.
	h.second.member.Explicit = false
	claimOne(t, h.second, key("a"), now.Add(time.Minute))
	if m := member(); m.Explicit {
		t.Error("a changed registration was not rewritten")
	}
	// A day later, unchanged, it is renewed.
	claimOne(t, h.second, key("b"), now.Add(25*time.Hour))
	if m := member(); m.SeenAtMS <= first.SeenAtMS+int64(24*time.Hour/time.Millisecond) {
		t.Errorf("a day-old registration was not renewed: seen %d, first %d", m.SeenAtMS, first.SeenAtMS)
	}

	write(t, filepath.Join(h.ownerRoot, "state.json"), `{"version":1,"members":"garbage"}`)
	if p := h.owner.Load().Problems(); len(p) != 1 || p[0].Section != "members" {
		t.Fatalf("problems = %+v", p)
	}
	claimOne(t, h.second, key("c"), now.Add(26*time.Hour))
	if p := h.owner.Load().Problems(); len(p) != 0 {
		t.Errorf("the section was not rebuilt: %+v", p)
	}
	member()
	if raw := readRaw(t, h.ownerRoot); raw["members_unreadable"] == nil {
		t.Error("the unreadable section was not set aside")
	}
}
