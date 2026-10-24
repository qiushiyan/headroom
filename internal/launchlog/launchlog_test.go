package launchlog

// The file protocol — appends, the rewrite that bounds the file, torn lines —
// is jsonl's, and is proven in jsonl_test. This file owns what a launch line
// carries.

import (
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/placement"
)

// record is a launch logged as placeLaunch logs it, and the decision it was
// made from: the candidates, record of launches and intent beside it.
func record(now time.Time, intent placement.Intent) (Record, placement.Decision) {
	cands := []placement.Candidate{
		{Name: "a", Key: "uuid:a", ObservedAt: now.Add(-time.Minute).Unix(), Source: "headroom_cache",
			Limits: []placement.Limit{
				{Kind: "session", Label: "5h session", Percent: 34, ResetAt: now.Add(time.Hour).Unix(), Session: true},
				{Kind: "weekly_all", Label: "All models (7d)", Percent: 12},
			},
			Busy: []placement.Proc{{PID: 9, StartedMS: 1, Home: "/h"}}, Statuses: []string{"busy", "shell"}},
		{Name: "b", Key: "uuid:b", Excluded: "not logged in"},
	}
	ledger := placement.Ledger{
		Recent: []placement.Pending{{Key: "uuid:b", Name: "b", PID: 3, AtMS: now.Add(-time.Minute).UnixMilli() + 7, Home: "/h"}},
		Last:   map[string]placement.Last{"uuid:a": {Name: "a", AtMS: now.Add(-time.Hour).UnixMilli() + 250}},
	}
	d := placement.Choose(cands, ledger, intent, now)
	return New(d, cands, ledger, intent, now), d
}

// A line carries what the rule answered, the figures it counted, and every
// input it read, through a write and a read: the times a reader compares in
// their formats, and lists as lists — never null — when they are empty.
func TestRecordCarriesEveryInput(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 1, 10, 2, 11, 0, time.UTC)
	rec, d := record(now, placement.Intent{Owner: "a", Exclude: "c"})
	rec.Vendor, rec.PID, rec.CWD, rec.Mode, rec.Recorded = "claude", 4242, "/tmp/p", "auto", true
	if err := Append(root, rec); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Read(root)
	if err != nil || skipped != 0 || len(got) != 1 {
		t.Fatalf("read: %d records, %d skipped, %v", len(got), skipped, err)
	}
	r := got[0]
	if r.At != "2026-10-01T10:02:11Z" || r.AtMS != now.UnixMilli() || !r.Recorded {
		t.Errorf("record = %+v", r)
	}
	// The answer is the rule's, as it gave it.
	if d.Chosen == "" || r.Chosen != d.Chosen || r.Reason != d.Reason || r.Rule != d.Rule {
		t.Errorf("logged %q/%q/%q, the rule answered %+v", r.Chosen, r.Reason, r.Rule, d)
	}
	// What the rule was asked travels with what it answered, so the line can
	// be decided again.
	if r.V != Version || !r.Automatic || r.Owner != "a" || r.Exclude != "c" {
		t.Errorf("intent = v%d automatic %v owner %q exclude %q", r.V, r.Automatic, r.Owner, r.Exclude)
	}
	if forced, _ := record(now, placement.Intent{Kind: placement.Forced, Account: "a"}); forced.Automatic {
		t.Error("a forced launch logged as the rule's choice")
	}
	a, b := r.Candidates[0], r.Candidates[1]
	if a.Key != "uuid:a" || b.Key != "uuid:b" {
		t.Errorf("keys = %q, %q", a.Key, b.Key)
	}
	// The figures are the ones the rule counted. The fixture's are all
	// nonzero, so a field the line left unset cannot pass for one.
	da := d.Candidates[0]
	if da.Load == 0 || da.Week.Counted == 0 || da.Limits[0].Counted == 0 {
		t.Fatalf("fixture: the rule counted %+v", da)
	}
	if a.Load != da.Load || a.Busy != da.Busy || a.Pending != da.Pending || a.Weekly != da.Week.Counted || a.NearLimit != da.NearLimit ||
		a.Week.Room != da.Week.Room || a.Week.LeftS != da.Week.Left || a.Week.LeftBasis != string(da.Week.LeftBasis) {
		t.Errorf("candidate a = %+v, the rule counted %+v", a, da)
	}
	for i, l := range a.Limits {
		if dl := da.Limits[i]; l.Percent != dl.Percent || l.Session != dl.Session || l.Counted != dl.Counted || l.Basis != string(dl.Basis) {
			t.Errorf("limit %d = %+v, the rule counted %+v", i, l, dl)
		}
	}
	if !a.Eligible || b.Eligible || b.Excluded != "not logged in" || a.Source != "headroom_cache" {
		t.Errorf("candidates = %+v, %+v", a, b)
	}
	// The raw input, not only the counts: the busy process, the record of
	// launches, and the last placement to the millisecond that breaks ties.
	if len(a.BusyProcs) != 1 || a.BusyProcs[0] != (Proc{PID: 9, StartedMS: 1, Home: "/h"}) || b.BusyProcs == nil {
		t.Errorf("busy procs = %+v, %+v", a.BusyProcs, b.BusyProcs)
	}
	if len(r.Recent) != 1 || r.Recent[0].PID != 3 || r.Recent[0].AtMS != now.Add(-time.Minute).UnixMilli()+7 {
		t.Errorf("recent = %+v", r.Recent)
	}
	if a.LastPlaced == nil || *a.LastPlaced != "2026-10-01T09:02:11.250Z" {
		t.Errorf("last placed = %v", a.LastPlaced)
	}
	if strings.Join(a.Statuses, ",") != "busy,shell" || b.Statuses == nil {
		t.Errorf("statuses = %v, %v", a.Statuses, b.Statuses)
	}
	if a.ObservedAt == nil || b.ObservedAt != nil || a.Limits[0].ResetsAt == nil {
		t.Errorf("observed %v / %v, reset %v", a.ObservedAt, b.ObservedAt, a.Limits[0].ResetsAt)
	}
	if a.Limits[1].ResetsAt != nil {
		t.Error("a row with no reset must say null, not 1970")
	}
}
