package accountstate

import (
	"testing"

	"github.com/qiushiyan/headroom/internal/usage"
)

// Actionable is whether the figures are grounds for choosing the account now:
// it is usable, its figures are fresh, and the vendor does not block it. Only
// positive blocking evidence blocks: unknown and bad never do, and a block on
// one feature leaves the account grounds for a choice. Fresh figures are not
// enough on their own — a logged-out account can hold a cache minutes old.
func TestActionableHonoursTheAllowance(t *testing.T) {
	now := int64(1_790_000_000)
	for _, c := range []struct {
		name      string
		health    Health
		age       int64 // seconds
		allowance usage.Allowance
		want      bool
	}{
		{"unknown — every Claude Code response", HealthOK, 5, usage.Allowance{}, true},
		{"allowed", HealthOK, 5, usage.Allowance{State: usage.AllowanceAllowed}, true},
		{"bad never blocks", HealthOK, 5, usage.Allowance{State: usage.AllowanceBad}, true},
		{"blocked, whatever the percentages say", HealthOK, 5, usage.Allowance{State: usage.AllowanceBlocked, Reason: "spend_control.reached"}, false},
		{"a feature-only block", HealthOK, 5, usage.Allowance{State: usage.AllowanceAllowed, BlockedFeatures: []string{"code review"}}, true},
		{"logged out over fresh figures", HealthNoLogin, 5, usage.Allowance{}, false},
		{"figures too old to choose on", HealthOK, 100000, usage.Allowance{}, false},
	} {
		v := Facts{Health: c.health, Obs: &Observation{
			Rows:       []usage.Row{{Percent: 3, ResetAt: now + 3600}},
			Allowance:  c.allowance,
			ObservedAt: now - c.age,
		}}
		if got := v.Actionable(now); got != c.want {
			t.Errorf("%s: Actionable = %v, want %v", c.name, got, c.want)
		}
		if got := v.Blocked(); got != (c.allowance.State == usage.AllowanceBlocked) {
			t.Errorf("%s: Blocked = %v", c.name, got)
		}
	}
	if (Facts{Health: HealthOK}).Blocked() {
		t.Error("no observation read as blocked")
	}
}

// The fresh window follows the scope's spacing, not one global constant.
func TestFreshFollowsTheFactsWindow(t *testing.T) {
	now := int64(1_790_000_000)
	obs := &Observation{ObservedAt: now - 200}
	if (Facts{Obs: obs}).Fresh(now) {
		t.Error("200s old read as fresh under the default window")
	}
	if !(Facts{Obs: obs, FreshFor: 300_000_000_000}).Fresh(now) {
		t.Error("200s old read as stale under a 300s spacing")
	}
}
