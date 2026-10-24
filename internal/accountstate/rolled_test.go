package accountstate

import (
	"testing"

	"github.com/qiushiyan/headroom/internal/usage"
)

// A window whose own reset instant has passed is not grounds for a choice,
// however recently it was observed: the observation is current, only the
// window ended, and its low percent reads as headroom that may not exist.
// (Moved here from render, which owns how such a row is drawn; deleting
// `!v.RolledOver(now)` from Actionable must fail this.)
func TestRolledOverWindowIsNotActionable(t *testing.T) {
	now := int64(1_790_000_000)
	rolled := usage.Row{Label: "5h session", Percent: 8, ResetAt: now - 33*3600}
	v := Facts{
		Health: HealthOK,
		Obs:    &Observation{Rows: []usage.Row{rolled}, ObservedAt: now - 5, Source: SourceLive},
	}
	if !v.Fresh(now) {
		t.Fatal("the observation itself is current; only the window ended")
	}
	if v.Actionable(now) {
		t.Error("an account whose window rolled over must not be offered as a live choice")
	}
}
