package launchlog

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
)

func decision(now time.Time) placement.Decision {
	cands := []placement.Candidate{
		{Name: "a", Key: "uuid:a", ObservedAt: now.Add(-time.Minute).Unix(), Source: "headroom_cache",
			Limits: []placement.Limit{
				{Kind: "session", Label: "5h session", Percent: 34, ResetAt: now.Add(time.Hour).Unix(), Session: true},
				{Kind: "weekly_all", Label: "All models (7d)", Percent: 12},
			},
			Busy: []placement.Proc{{PID: 9, StartedMS: 1}}, Statuses: []string{"busy", "shell"}},
		{Name: "b", Key: "uuid:b", Excluded: "not logged in"},
	}
	return placement.Choose(cands, placement.Ledger{}, placement.Intent{}, now)
}

func TestRecordCarriesEveryInput(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 1, 10, 2, 11, 0, time.UTC)
	rec := New(decision(now), now)
	rec.Vendor, rec.PID, rec.CWD, rec.Mode, rec.Recorded = "claude", 4242, "/tmp/p", "auto", true
	if err := Append(root, rec); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Read(root, 0)
	if err != nil || skipped != 0 || len(got) != 1 {
		t.Fatalf("read: %d records, %d skipped, %v", len(got), skipped, err)
	}
	r := got[0]
	if r.At != "2026-10-01T10:02:11Z" || r.Chosen != "a" || r.Reason != placement.ReasonLeastLoad || r.Rule != placement.Rule || !r.Recorded {
		t.Errorf("record = %+v", r)
	}
	a, b := r.Candidates[0], r.Candidates[1]
	if !a.Eligible || a.Load != 4 || a.Busy != 1 || a.Weekly != 12 || a.Source != "headroom_cache" ||
		strings.Join(a.Statuses, ",") != "busy,shell" || a.ObservedAt == nil {
		t.Errorf("candidate a = %+v", a)
	}
	if l := a.Limits[0]; !l.Session || l.Counted != 34 || l.Basis != "observed" || l.ResetsAt == nil {
		t.Errorf("limit = %+v", l)
	}
	if a.Limits[1].ResetsAt != nil {
		t.Error("a row with no reset must say null, not 1970")
	}
	if b.Eligible || b.Excluded != "not logged in" || b.ObservedAt != nil || b.Statuses == nil {
		t.Errorf("candidate b = %+v", b)
	}
}

func TestATornLineIsSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for range 3 {
		if err := Append(root, New(decision(now), now)); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.OpenFile(Path(root), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"v":1,"at":"2026-10-01T10:0`)
	f.Close()
	got, skipped, err := Read(root, 2)
	if err != nil || len(got) != 2 || skipped != 1 {
		t.Fatalf("read: %d records, %d skipped, %v", len(got), skipped, err)
	}
	if got, _, err := Read(t.TempDir(), 5); err != nil || got != nil {
		t.Fatalf("an absent log is an empty one: %v %v", got, err)
	}
}

// Bounded by age, and only above the size bound: below it nothing is ever
// rewritten, and above it only what is old goes.
func TestBoundedByAgeOnlyAboveTheSizeBound(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := New(decision(now), now.Add(-Keep-time.Hour))
	if err := Append(root, old); err != nil {
		t.Fatal(err)
	}
	if err := Append(root, New(decision(now), now)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := Read(root, 0); len(got) != 2 {
		t.Fatalf("below the size bound an old line was dropped: %d left", len(got))
	}

	prev := MaxBytes
	MaxBytes = 1
	t.Cleanup(func() { MaxBytes = prev })

	// The log's own lock held alone by someone else — a rewrite in progress:
	// the append gives up within its wait and says so, rather than writing to
	// a file that is about to be replaced.
	lock, err := os.OpenFile(Path(root)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := Append(root, New(decision(now), now)); err != ErrBusy {
		t.Fatalf("append under a held lock: %v, want ErrBusy", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("an append waited on the log's lock past its bound")
	}
	if got, _, _ := Read(root, 0); len(got) != 2 {
		t.Fatalf("with the lock held: %d records, want the 2 already there", len(got))
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()

	if err := Append(root, New(decision(now), now)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Read(root, 0)
	if len(got) != 2 {
		t.Fatalf("above the bound: %d records, want the two recent ones", len(got))
	}
	for _, r := range got {
		if r.At == old.At {
			t.Fatal("the old line survived")
		}
	}
}

// A damaged tail — a line some writer never finished — must not swallow the
// next record: a launch that succeeded is a line that can be read back.
func TestADamagedTailDoesNotSwallowTheNextRecord(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	if err := os.WriteFile(Path(root), []byte(`{"v":1,"at":"2026-10-01T10:0`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Append(root, New(decision(now), now)); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Read(root, 0)
	if err != nil || len(got) != 1 || skipped != 1 {
		t.Fatalf("after a damaged tail: %d records, %d skipped, %v", len(got), skipped, err)
	}
}

// Appends and the rewrite that bounds the file share one protocol: a record
// whose append succeeded is in the file, however many rewrites ran meanwhile.
// An append may give up under contention — and says so; what it may not do is
// report success for a line the rename then threw away.
func TestAppendsSurviveConcurrentPruning(t *testing.T) {
	root := t.TempDir()
	prev := MaxBytes
	MaxBytes = 1 // every append is "above the bound"
	t.Cleanup(func() { MaxBytes = prev })

	now := time.Now()
	const writers, each = 8, 40
	var wg sync.WaitGroup
	var written atomic.Int32
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				// An old line beside every new one, so each rewrite has
				// something to drop and really does rename.
				old := New(decision(now), now.Add(-Keep-time.Hour))
				old.PID = -1
				_ = Append(root, old)
				rec := New(decision(now), now)
				rec.PID = w*1000 + i
				switch err := Append(root, rec); err {
				case nil:
					written.Add(1)
				case ErrBusy:
				default:
					t.Errorf("append: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	got, skipped, err := Read(root, 0)
	if err != nil || skipped != 0 {
		t.Fatalf("read: %d skipped, %v", skipped, err)
	}
	kept := 0
	for _, r := range got {
		if r.PID >= 0 {
			kept++
		}
	}
	if int(written.Load()) == 0 || kept != int(written.Load()) {
		t.Fatalf("%d appends reported success and %d of their records are in the file", written.Load(), kept)
	}
}
