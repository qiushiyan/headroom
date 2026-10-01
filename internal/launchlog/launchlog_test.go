package launchlog

import (
	"os"
	"strings"
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

	// The log's own lock held by someone else: the append lands, nothing is
	// rewritten, and nobody waits.
	lock, err := os.OpenFile(Path(root)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := Append(root, New(decision(now), now)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("an append waited on the log's lock")
	}
	if got, _, _ := Read(root, 0); len(got) != 3 {
		t.Fatalf("with the lock held: %d records, want 3", len(got))
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()

	if err := Append(root, New(decision(now), now)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Read(root, 0)
	if len(got) != 3 {
		t.Fatalf("above the bound: %d records, want the three recent ones", len(got))
	}
	for _, r := range got {
		if r.At == old.At {
			t.Fatal("the old line survived")
		}
	}
}
