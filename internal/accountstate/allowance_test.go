package accountstate

import (
	"testing"

	"github.com/qiushiyan/headroom/internal/usage"
)

// Actionable consults the allowance. Only positive blocking evidence blocks:
// unknown and bad never do, and a block on one feature leaves the account
// grounds for a choice.
func TestActionableHonoursTheAllowance(t *testing.T) {
	now := int64(1_790_000_000)
	for _, c := range []struct {
		name      string
		allowance usage.Allowance
		want      bool
	}{
		{"unknown — every Claude Code response", usage.Allowance{}, true},
		{"allowed", usage.Allowance{State: usage.AllowanceAllowed}, true},
		{"bad never blocks", usage.Allowance{State: usage.AllowanceBad}, true},
		{"blocked, whatever the percentages say", usage.Allowance{State: usage.AllowanceBlocked, Reason: "spend_control.reached"}, false},
		{"a feature-only block", usage.Allowance{State: usage.AllowanceAllowed, BlockedFeatures: []string{"code review"}}, true},
	} {
		v := Facts{Health: HealthOK, Obs: &Observation{
			Rows:       []usage.Row{{Percent: 3, ResetAt: now + 3600}},
			Allowance:  c.allowance,
			ObservedAt: now - 5,
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
