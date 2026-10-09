package usagelog

import (
	"reflect"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/usage"
)

func rows(session int) []usage.Row {
	return []usage.Row{
		{Kind: "session", Label: "5h session", Percent: session, ResetAt: 1791580000,
			PercentState: usage.StateOK, ResetState: usage.StateOK, IdentityState: usage.StateOK},
		{Kind: "weekly_scoped", Model: "Opus", Label: "Opus (7d)", Percent: 0,
			PercentState: usage.StateBad, ResetState: usage.StateNone, IdentityState: usage.StateOK},
		{Kind: "codex", Group: "main", WindowSeconds: 18000, Label: "5h", Unstarted: true,
			PercentState: usage.StateOK, ResetState: usage.StateNone, IdentityState: usage.StateBad},
	}
}

// What is logged reads back as the row the parser produced, so the vendor's
// rules see a logged row as they saw the live one.
func TestARowReadsBackAsTheParserProducedIt(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)
	in := rows(34)
	if err := Append(root, New(at, config.Claude, "uuid:a", "a", "/h", in, usage.Allowance{})); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Read(root)
	if err != nil || skipped != 0 || len(got) != 1 {
		t.Fatalf("read: %d records, %d skipped, %v", len(got), skipped, err)
	}
	r := got[0]
	if r.At != "2026-10-09T20:00:00Z" || r.Key != "uuid:a" || r.Name != "a" || r.Home != "/h" || r.Vendor != "claude" || r.Allowance != "" {
		t.Errorf("record = %+v", r)
	}
	for i, row := range r.Rows {
		if back := row.Usage(); !reflect.DeepEqual(back, in[i]) {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, back, in[i])
		}
	}
	codex := New(at, config.Codex, "uuid:x/u", "x", "/h", nil, usage.Allowance{State: usage.AllowanceBlocked})
	if codex.Allowance != "blocked" || codex.Rows == nil {
		t.Errorf("codex record = %+v", codex)
	}
}

// An unchanged reading is logged once per heartbeat; a changed one, or another
// account's, at once.
func TestAnUnchangedReadingWaitsForTheHeartbeat(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)
	add := func(at time.Time, key string, session int) {
		t.Helper()
		if err := Append(root, New(at, config.Claude, key, key, "/h", rows(session), usage.Allowance{})); err != nil {
			t.Fatal(err)
		}
	}
	add(t0, "uuid:a", 10)
	add(t0.Add(time.Minute), "uuid:a", 10)             // same: skipped
	add(t0.Add(time.Minute), "uuid:b", 10)             // another account
	add(t0.Add(2*time.Minute), "uuid:a", 11)           // changed
	add(t0.Add(3*time.Minute), "uuid:a", 11)           // same: skipped
	add(t0.Add(2*time.Minute+Heartbeat), "uuid:a", 11) // a heartbeat on
	got, _, _ := Read(root)
	var at []string
	for _, r := range got {
		at = append(at, r.Key+"@"+r.At[11:16])
	}
	want := []string{"uuid:a@20:00", "uuid:b@20:01", "uuid:a@20:02", "uuid:a@20:17"}
	if !reflect.DeepEqual(at, want) {
		t.Errorf("logged %v, want %v", at, want)
	}
}
