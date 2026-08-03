package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/usage"
)

func key(name string) Key { return Key{Name: name} }

func claimOne(t *testing.T, s *Store, k Key, now time.Time) Decision {
	t.Helper()
	dec, err := s.Claim([]Key{k}, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return dec[0]
}

func readRaw(t *testing.T, root string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("state.json is not an object: %v", err)
	}
	return m
}

// The headline invariant: eligibility and the claim are one locked operation,
// so two processes cannot both conclude an account is free to fetch. flock is
// per open file description, so two Store handles in one process contend for
// the real lock exactly as two processes would.
func TestClaimIsTestAndSet(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	const racers = 8

	var wg sync.WaitGroup
	permits := make([]bool, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := Open(root)
			<-start
			dec, _ := s.Claim([]Key{key("a")}, now)
			permits[i] = dec[0].Permit
		}(i)
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
		t.Errorf("%d of %d racers were permitted; exactly one may spend the budget", granted, racers)
	}
}

// Two config dirs logged into the same account share one endpoint bucket, so
// they must share one ledger record — keying by dir name would let them
// double-spend it invisibly.
func TestIdentityKeyingSharesOneBucket(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now()
	a := Key{UUID: "same-uuid", Name: "work@x.com"}
	b := Key{UUID: "same-uuid", Name: "personal@x.com"}

	dec, err := s.Claim([]Key{a, b}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !dec[0].Permit || dec[1].Permit {
		t.Errorf("two dirs on one account got %v and %v; they share a bucket",
			dec[0].Permit, dec[1].Permit)
	}
	// And with no UUID they are genuinely separate accounts again.
	c, d := key("one@x.com"), key("two@x.com")
	dec, _ = s.Claim([]Key{c, d}, now)
	if !dec[0].Permit || !dec[1].Permit {
		t.Error("distinct accounts must not share a quiet period")
	}
}

func TestClaimDeniesInsideTheQuietPeriod(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now()
	if !claimOne(t, s, key("a"), now).Permit {
		t.Fatal("first claim must be granted")
	}
	dec := claimOne(t, s, key("a"), now.Add(time.Second))
	if dec.Permit {
		t.Error("a second claim inside the spacing must be denied")
	}
	if !dec.NextEligible.After(now) {
		t.Error("a denial must say when the account becomes eligible")
	}
	if !claimOne(t, s, key("a"), now.Add(usage.RequestSpacing+time.Second)).Permit {
		t.Error("the quiet period must end")
	}
}

// A fetch can outlive the claim that authorized it — the client allows ten
// seconds and another process can claim, fetch and record inside that. The
// straggler must not overwrite the newer observation or reset its cooldown.
func TestStaleCompletionIsDropped(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now()
	k := key("a")
	stale := claimOne(t, s, k, now).Generation

	fresh := claimOne(t, s, k, now.Add(2*usage.RequestSpacing)).Generation
	if _, err := s.Complete(k, fresh, OutcomeStored,
		[]byte(`{"limits":[{"kind":"session","percent":10}]}`), now.Add(2*usage.RequestSpacing)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(k, stale, OutcomeStored,
		[]byte(`{"limits":[{"kind":"session","percent":99}]}`), now.Add(3*usage.RequestSpacing)); err != nil {
		t.Fatal(err)
	}

	obs, ok := s.Load().Observation(k, now.Add(3*usage.RequestSpacing))
	if !ok {
		t.Fatal("no observation stored")
	}
	rows, err := usage.ParseLimits(obs.Body)
	if err != nil || len(rows) != 1 || rows[0].Percent != 10 {
		t.Errorf("a late answer overwrote a newer one: %v %v", rows, err)
	}
}

// The stored body must come back with every value intact. Decoding and
// re-marshalling it would push numeric epoch timestamps through float64 — a
// silent mutation of vendor data by the one component that promised not to
// interpret it. Whitespace is the encoder's to normalize; values are not.
func TestStoredBodyKeepsEveryValue(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now()
	k := key("a")
	// A reset epoch and an id past float64's exact-integer range: both would
	// come back altered if anything decoded them into a number.
	body := []byte(`{"limits":[{"kind":"session","percent":8,"resets_at":1785622528}],` +
		`"trace":9007199254740993}`)

	dec := claimOne(t, s, k, now)
	if _, err := s.Complete(k, dec.Generation, OutcomeStored, body, now); err != nil {
		t.Fatal(err)
	}
	obs, ok := s.Load().Observation(k, now)
	if !ok {
		t.Fatal("no observation stored")
	}
	if got, want := compact(t, obs.Body), compact(t, body); got != want {
		t.Errorf("body was altered:\n got %s\nwant %s", got, want)
	}
	rows, err := usage.ParseLimits(obs.Body)
	if err != nil || len(rows) != 1 || rows[0].ResetAt != 1785622528 {
		t.Errorf("numeric epoch did not survive: %+v %v", rows, err)
	}
}

// A 200 that did not parse still proves the budget recovered, but leaves
// nothing worth keeping — and an oversized body is dropped rather than making
// every future claim pay to rewrite it.
func TestOnlyUsableBodiesAreStored(t *testing.T) {
	now := time.Now()
	k := key("a")

	s := Open(t.TempDir())
	dec := claimOne(t, s, k, now)
	if _, err := s.Complete(k, dec.Generation, OutcomeSpent, []byte(`nonsense`), now); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load().Observation(k, now); ok {
		t.Error("an unparseable body was stored")
	}

	s = Open(t.TempDir())
	dec = claimOne(t, s, k, now)
	huge := make([]byte, BodyLimit+1)
	if _, err := s.Complete(k, dec.Generation, OutcomeStored, huge, now); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load().Observation(k, now); ok {
		t.Error("a body over the limit was stored")
	}
}

// Wall clock is the only axis available across processes. A step backwards
// must not leave a two-day-old number reading as current until the clock
// catches up, and a step forwards must not silence an account for as long as
// the step.
func TestClockAnomaliesAreNotTrusted(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	k := key("a")
	s := Open(root)
	dec := claimOne(t, s, k, now)
	if _, err := s.Complete(k, dec.Generation, OutcomeStored,
		[]byte(`{"limits":[]}`), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load().Observation(k, now); ok {
		t.Error("an observation stamped in the future was treated as data")
	}
	if _, ok := s.Load().Observation(k, now.Add(2*time.Hour)); !ok {
		t.Error("once the clock catches up the observation is ordinary again")
	}

	// A next-eligible further out than this code can produce is a clock step.
	writeDoc(t, root, `{"version":1,"accounts":{"dir:b":{"request":{"next_eligible_ms":`+
		itoa(now.Add(72*time.Hour).UnixMilli())+`}}}}`)
	got := Open(root).Load().NextEligible(key("b"), now)
	if got.After(now.Add(CooldownMax + time.Second)) {
		t.Errorf("absurd deadline not clamped: %v out", got.Sub(now))
	}
}

// A section this binary cannot decode is carried through a write by a section
// it can. The trap: a routine usage claim rewriting the document must not
// destroy the user's re-homes, which are the one thing here that cannot be
// re-derived.
func TestUnreadableSectionSurvivesAnUnrelatedWrite(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"sessions":"not-an-object","extra":{"future":true}}`)
	s := Open(root)

	if s.Load().OwnersReadable() {
		t.Fatal("a bad sessions section must not read as readable")
	}
	if !claimOne(t, s, key("a"), time.Now()).Permit {
		t.Fatal("an unrelated section must not block the ledger")
	}

	raw := readRaw(t, root)
	if string(raw["sessions"]) != `"not-an-object"` {
		t.Errorf("re-homes were destroyed by a usage refresh: %s", raw["sessions"])
	}
	if compact(t, raw["extra"]) != `{"future":true}` {
		t.Errorf("an unknown section was dropped: %s", raw["extra"])
	}
	// And mutations against it are refused rather than silently rebuilding it.
	if err := s.ReHome("s1", "a@x.com", time.Now(), nil); err != ErrCorrupt {
		t.Errorf("ReHome over a corrupt section: %v", err)
	}
	if err := s.Forget("s1"); err != ErrCorrupt {
		t.Errorf("Forget over a corrupt section: %v", err)
	}
}

// The ledger is disposable, so it is rebuilt rather than refused — but with no
// readable record, "eligible" is a guess, and a live cooldown is exactly when
// that guess is worst. It starts quiet and self-heals.
func TestUnreadableLedgerIsQuarantinedAndQuiet(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"accounts":5}`)
	s := Open(root)
	now := time.Now()

	dec := claimOne(t, s, key("a"), now)
	if dec.Permit {
		t.Error("traffic was issued on an unreadable ledger")
	}
	raw := readRaw(t, root)
	if string(raw["accounts_unreadable"]) != "5" {
		t.Errorf("the unreadable bytes were dropped instead of set aside: %v", raw)
	}
	if !claimOne(t, Open(root), key("a"), now.Add(CooldownMax+time.Minute)).Permit {
		t.Error("the quarantine must self-heal after one cooldown")
	}
}

// A document that is not JSON at all cannot be recovered section by section,
// but it is still not this code's to destroy.
func TestUnreadableDocumentIsKept(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{ this is not json`)
	s := Open(root)
	if len(s.Load().Problems()) == 0 {
		t.Error("a corrupt document must be reported")
	}
	if _, err := s.Claim([]Key{key("a")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "state.json.unreadable")); err != nil {
		t.Errorf("the corrupt bytes were overwritten: %v", err)
	}
	readRaw(t, root) // the replacement is a valid document
}

// An older binary must never rewrite a newer schema: it would silently
// truncate everything it did not understand.
func TestNewerSchemaIsReadOnly(t *testing.T) {
	root := t.TempDir()
	doc := `{"version":999,"accounts":{},"tomorrow":{"kept":1}}`
	writeDoc(t, root, doc)
	s := Open(root)

	if !s.Load().ReadOnly() {
		t.Fatal("a newer schema must be reported read-only")
	}
	if _, err := s.Claim([]Key{key("a")}, time.Now()); err != ErrReadOnly {
		t.Errorf("Claim: %v, want ErrReadOnly", err)
	}
	if err := s.ReHome("s1", "a@x.com", time.Now(), nil); err != ErrReadOnly {
		t.Errorf("ReHome: %v, want ErrReadOnly", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil || string(got) != doc {
		t.Errorf("the document was rewritten: %s", got)
	}
	// Nothing may be fetched on a ledger this binary is not allowed to claim
	// against, so eligibility reads as a full cooldown out.
	now := time.Now()
	if !s.Load().NextEligible(key("a"), now).After(now) {
		t.Error("a read-only store must not report an account as eligible")
	}
}

// Upgrading while an account sits in a cooldown must not look like a clean
// slate: an empty ledger would fan out into a live rate limit, which is the
// failure the ledger exists to prevent.
func TestLegacyThrottleIsAFloorUntilItExpires(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	throttlePath := filepath.Join(root, ".throttle")
	write(t, throttlePath, `{"a":{"last_attempt_ms":1,"next_eligible_ms":`+
		itoa(now.Add(10*time.Minute).UnixMilli())+`,"strikes":3}}`)

	s := Open(root)
	if claimOne(t, s, key("a"), now).Permit {
		t.Error("a live legacy cooldown was ignored on upgrade")
	}
	if _, err := os.Stat(throttlePath); err != nil {
		t.Error(".throttle was retired while it still carried a live cooldown")
	}

	// Once every deadline it carries has passed it says nothing the document
	// does not, and goes.
	write(t, throttlePath, `{"a":{"last_attempt_ms":1,"next_eligible_ms":2}}`)
	if !claimOne(t, Open(root), key("a"), now.Add(time.Hour)).Permit {
		t.Error("an expired legacy floor must not keep denying")
	}
	if _, err := os.Stat(throttlePath); !os.IsNotExist(err) {
		t.Error(".throttle outlived its usefulness")
	}
}

// .owners carries user decisions, so migration keeps them and is idempotent:
// an older binary still running can write the file again afterwards, and the
// newest-wins merge absorbs it.
func TestLegacyOwnersAreAbsorbedAndRetired(t *testing.T) {
	root := t.TempDir()
	ownersPath := filepath.Join(root, ".owners")
	write(t, ownersPath, `{"owners":{"s1":{"account":"a@x.com","atMs":100}}}`)

	s := Open(root)
	if got := s.Load().Owners()["s1"].Account; got != "a@x.com" {
		t.Fatalf("legacy re-home not read: %q", got)
	}
	if _, err := s.Claim(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownersPath); !os.IsNotExist(err) {
		t.Error(".owners was not retired after migration")
	}
	if got := s.Load().Owners()["s1"].Account; got != "a@x.com" {
		t.Fatalf("re-home lost in migration: %q", got)
	}

	// A late write by an older binary, absorbed newest-wins on the next load
	// and folded into the document by the next write.
	write(t, ownersPath, `{"owners":{"s1":{"account":"b@x.com","atMs":200}}}`)
	if got := s.Load().Owners()["s1"].Account; got != "b@x.com" {
		t.Errorf("a late legacy write was ignored: %q", got)
	}
	if _, err := s.Claim(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	// An older record must not then resurrect over that newer decision.
	write(t, ownersPath, `{"owners":{"s1":{"account":"c@x.com","atMs":50}}}`)
	if got := s.Load().Owners()["s1"].Account; got != "b@x.com" {
		t.Errorf("an older legacy record displaced a newer one: %q", got)
	}
}

func TestReHomeAndForget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-yet")
	s := Open(root)
	now := time.UnixMilli(1000)

	// A fresh machine has no accounts root yet; the first re-home must create
	// it rather than fail and strand a routing the user asked for.
	if err := s.ReHome("s1", "a@x.com", now, nil); err != nil {
		t.Fatalf("re-home into a missing parent: %v", err)
	}
	if err := s.ReHome("s2", "b@x.com", now, nil); err != nil {
		t.Fatal(err)
	}
	m := s.Load().Owners()
	if m["s1"].Account != "a@x.com" || m["s2"].AtMS != 1000 {
		t.Fatalf("owners = %v", m)
	}
	if err := s.Forget("s2"); err != nil {
		t.Fatal(err)
	}
	if m = s.Load().Owners(); len(m) != 1 || m["s2"].Account != "" {
		t.Fatalf("after forget = %v", m)
	}
	if _, ok := s.Load().Owner("s1"); !ok {
		t.Error("Owner lookup lost a live record")
	}
}

// Records are bounded by age, never by which accounts a caller could see. The
// trap this replaced: sweeping by absence deletes a live cooldown the moment
// an account's key moves, and its key comes from a vendor file that can be
// read mid-write.
func TestTheLedgerIsBoundedByAgeAlone(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	now := time.Now()
	if _, err := s.Claim([]Key{key("old"), key("recent")}, now.Add(-retention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim([]Key{key("recent")}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A claim for neither of them still sweeps, and still keeps the one that
	// is merely absent from this call.
	if _, err := s.Claim([]Key{key("other")}, now); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range s.Load().Audit() {
		names[r.Name] = true
	}
	if names["old"] {
		t.Error("a record untouched for a month was kept")
	}
	if !names["recent"] || !names["other"] {
		t.Errorf("a live record was swept by a call that did not name it: %v", names)
	}
}

// The write gate: a run where every account was inside its quiet period
// changes nothing, and rewriting the document to say so would be pure
// contention between surfaces that all start at once.
func TestADeniedRoundWritesNothing(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	now := time.Now()
	claimOne(t, s, key("a"), now)

	path := filepath.Join(root, "state.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if claimOne(t, s, key("a"), now.Add(time.Second)).Permit {
		t.Fatal("the second claim was permitted inside the quiet period")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a denied round rewrote the document")
	}
}

func writeDoc(t *testing.T, root, content string) {
	t.Helper()
	write(t, filepath.Join(root, "state.json"), content)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// compact normalizes whitespace so a comparison is about values, not layout.
func compact(t *testing.T, b []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatalf("compact %s: %v", b, err)
	}
	return out.String()
}

// A board redrawing every second must never block on another process holding
// the lock — a re-home's transcript walk is the longest critical section here.
// Acquisition is bounded, so contention becomes a rendered "state unavailable"
// and a retry next round, never a frozen picker.
func TestLockAcquisitionIsBounded(t *testing.T) {
	root := t.TempDir()
	held, err := os.OpenFile(filepath.Join(root, "state.json.lock"),
		os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(held.Fd()), syscall.LOCK_UN)

	start := time.Now()
	dec, err := Open(root).Claim([]Key{key("a")}, time.Now())
	elapsed := time.Since(start)

	if err != ErrBusy {
		t.Errorf("Claim under a held lock: %v, want ErrBusy", err)
	}
	if dec[0].Permit {
		t.Error("traffic was authorized by a claim that could not be written")
	}
	if elapsed > claimWait+time.Second {
		t.Errorf("blocked for %v; a claim must give up quickly — nothing is lost by "+
			"trying again next round", elapsed)
	}
}

// A JSON null decodes into a map without error and leaves the map nil, so a
// section written as `null` passes every readability check this package has
// and then panics the first write that touches it. Vendor files this tool
// reads are full of nulls; its own must survive one.
func TestNullSectionsAreNotNilMaps(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"accounts":null,"sessions":null}`)
	s := Open(root)

	if !claimOne(t, s, key("a"), time.Now()).Permit {
		t.Error("a null ledger must read as an empty one, not block the claim")
	}
	if err := s.ReHome("s1", "a@x.com", time.Now(), nil); err != nil {
		t.Errorf("re-home over a null sessions section: %v", err)
	}
}

// The section that holds human decisions is validated whole, exactly as the
// .owners reader it replaced was: a record decoding around empty fields would
// have `check` report the store readable while routing silently ignores it.
func TestHollowRehomeRecordsAreRefused(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"sessions":{"s1":{"account":"a@x.com","atMs":1},"s2":{}}}`)
	snap := Open(root).Load()

	if snap.OwnersReadable() {
		t.Error("a section with a hollow record must not report itself readable")
	}
	if len(snap.Problems()) == 0 {
		t.Error("the loss must be reportable by check")
	}
}

// A file that exists but cannot be read is not an absent file. Treating the
// two alike lets the next mutation write a fresh document over re-homes that
// are still there — the one thing in here that cannot be re-derived.
func TestAnUnreadableFileIsNotAnEmptyStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through the permission bits")
	}
	root := t.TempDir()
	writeDoc(t, root, `{"version":1,"sessions":{"s1":{"account":"a@x.com","atMs":1}}}`)
	path := filepath.Join(root, "state.json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	s := Open(root)

	if err := s.ReHome("s2", "b@x.com", time.Now(), nil); err != ErrUnreadable {
		t.Errorf("mutation against a document that could not be read: %v", err)
	}
	if dec, _ := s.Claim([]Key{key("a")}, time.Now()); dec[0].Permit {
		t.Error("traffic was authorized on a ledger that could not be read")
	}
	os.Chmod(path, 0o600)
	if _, ok := Open(root).Load().Owner("s1"); !ok {
		t.Error("the re-home was destroyed by a write that treated the file as absent")
	}
}
