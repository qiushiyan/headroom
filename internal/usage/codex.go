package usage

import (
	"encoding/json"
	"fmt"
	"math"
)

// The Codex usage body (GET …/backend-api/wham/usage; observed on codex-cli
// 0.155.0, see DESIGN.md § The system observed):
//
//	{"plan_type":"pro","account_id":…,"user_id":…,"email":…,
//	 "rate_limit":{"allowed":true,"limit_reached":false,
//	   "primary_window":{"used_percent":93,"limit_window_seconds":604800,
//	     "reset_after_seconds":259060,"reset_at":1790239759},
//	   "secondary_window":null},
//	 "code_review_rate_limit":null,
//	 "additional_rate_limits":null,
//	 "spend_control":{"reached":false,…},
//	 "rate_limit_reached_type":null, …}
//
// An additional_rate_limits entry is {limit_name, metered_feature,
// rate_limit}, its rate_limit shaped like the top-level one (from the vendor's
// source; never seen live). Credit balances and reset-credit counts are parsed
// by nobody in this version.
//
// A row's identity is the vendor's own words: Kind is the slot, Group the
// limit object the window sits in, Feature the metered feature of an
// additional limit. Model is always empty and Severity always "normal" — the
// vendor sends neither, and a reached limit is the allowance's to carry.

const (
	CodexGroupMain       = "rate_limit"
	CodexGroupCodeReview = "code_review_rate_limit"
	CodexGroupAdditional = "additional"
)

var codexSlots = []struct{ key, kind string }{
	{"primary_window", "primary"},
	{"secondary_window", "secondary"},
}

// parseCodex separates rejecting the envelope from degrading a field. The
// body is unparseable when it is not a JSON object or has no rate_limit key;
// everything inside degrades visibly, row by row, and is never dropped.
func parseCodex(body []byte) (Reading, error) {
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return Reading{}, ErrUnparseable
	}
	main, present := top["rate_limit"]
	if !present {
		return Reading{}, ErrUnparseable
	}
	r := Reading{Rows: []Row{}}
	r.AccountID, _ = top["account_id"].(string)
	r.UserID, _ = top["user_id"].(string)
	r.Plan, _ = top["plan_type"].(string)

	// A null rate_limit is an observation of no limits, as an empty limits[]
	// is for Claude Code.
	mainObj, mainOK := limitObject(main)
	if !mainOK {
		return Reading{}, ErrUnparseable
	}
	r.Rows = append(r.Rows, codexWindows(mainObj, CodexGroupMain, "", "")...)

	if review, ok := limitObject(top["code_review_rate_limit"]); !ok {
		r.Rows = append(r.Rows, badIdentityRow(CodexGroupCodeReview, "code review"))
	} else {
		r.Rows = append(r.Rows, codexWindows(review, CodexGroupCodeReview, "", "code review")...)
		if featureBlocked(review) {
			r.Allowance.BlockedFeatures = append(r.Allowance.BlockedFeatures, "code review")
		}
	}

	switch extra := top["additional_rate_limits"].(type) {
	case nil:
	case []any:
		for _, entry := range extra {
			e, ok := entry.(map[string]any)
			feature, _ := e["metered_feature"].(string)
			if !ok || feature == "" {
				// An entry that cannot say which feature it meters is
				// unselectable. One bad row, so `check` fails on it.
				r.Rows = append(r.Rows, badIdentityRow(CodexGroupAdditional, "additional limit"))
				continue
			}
			name, _ := e["limit_name"].(string)
			if name == "" {
				name = feature
			}
			limit, ok := limitObject(e["rate_limit"])
			if !ok {
				row := badIdentityRow(CodexGroupAdditional, name)
				row.Feature = feature
				r.Rows = append(r.Rows, row)
				continue
			}
			r.Rows = append(r.Rows, codexWindows(limit, CodexGroupAdditional, feature, name)...)
			if featureBlocked(limit) {
				r.Allowance.BlockedFeatures = append(r.Allowance.BlockedFeatures, name)
			}
		}
	default:
		r.Rows = append(r.Rows, badIdentityRow(CodexGroupAdditional, "additional limits"))
	}

	r.Allowance.State, r.Allowance.Reason = codexAllowance(top, mainObj)
	return r, nil
}

// limitObject decodes one rate-limit object. nil with ok=true is a
// legitimately absent limit (null or missing); ok=false is a value present
// under another type.
func limitObject(v any) (map[string]any, bool) {
	if v == nil {
		return nil, true
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func badIdentityRow(group, label string) Row {
	return Row{Label: label, Group: group, Severity: "normal",
		PercentState: StateBad, ResetState: StateNone, IdentityState: StateBad}
}

// codexWindows turns one limit object's slots into rows. A null or absent
// window contributes none.
func codexWindows(limit map[string]any, group, feature, suffix string) []Row {
	var rows []Row
	for _, slot := range codexSlots {
		v, present := limit[slot.key]
		if !present || v == nil {
			continue
		}
		row := Row{Kind: slot.kind, Group: group, Feature: feature, Severity: "normal"}
		w, ok := v.(map[string]any)
		if !ok {
			row.PercentState, row.ResetState, row.IdentityState = StateBad, StateNone, StateBad
			row.Label = codexLabel(0, suffix)
			rows = append(rows, row)
			continue
		}
		row.Percent, row.PercentState = codexPercent(w["used_percent"])
		row.ResetAt, row.ResetState = parseReset(w["reset_at"])
		if secs, ok := w["limit_window_seconds"].(float64); ok && secs > 0 {
			row.WindowSeconds = int64(secs)
		} else {
			// A window that cannot say how long it is cannot be told from a
			// sibling slot of another duration: unselectable.
			row.IdentityState = StateBad
		}
		// The unstarted predicate is deliberately narrow: every field must
		// decode, nothing may have been spent, and the remaining time must be
		// at least the whole window — which a started window fails after one
		// second. A missed unstarted window shows a full-window countdown,
		// which is harmless; a false "not started" would hide a real reset.
		if after, ok := w["reset_after_seconds"].(float64); ok &&
			row.PercentState == StateOK && row.ResetState == StateOK && row.IdentityState == StateOK &&
			row.Percent == 0 && int64(after) >= row.WindowSeconds {
			row.Unstarted, row.ResetAt, row.ResetState = true, 0, StateNone
		}
		row.Label = codexLabel(row.WindowSeconds, suffix)
		rows = append(rows, row)
	}
	return rows
}

// codexPercent is strict where Claude Code's is lenient: used_percent has
// only ever been a number, and a string there is drift.
func codexPercent(v any) (int, FieldState) {
	if n, ok := v.(float64); ok {
		return int(math.Round(n)), StateOK
	}
	return 0, StateBad
}

// codexLabel names the duration, then whatever the group adds. It derives
// from decoded fields of the same entry and nothing else.
func codexLabel(seconds int64, suffix string) string {
	var d string
	switch {
	case seconds <= 0:
		d = "?"
	case seconds == 7*86400:
		d = "weekly"
	case seconds%86400 == 0:
		d = fmt.Sprintf("%dd", seconds/86400)
	case seconds >= 3600:
		d = fmt.Sprintf("%dh", int64(math.Round(float64(seconds)/3600)))
	default:
		d = fmt.Sprintf("%dm", max(seconds/60, 1))
	}
	if suffix == "" {
		return d
	}
	return d + " " + suffix
}

// featureBlocked reports a limit object that is itself refusing: allowed
// false or limit_reached true, well-typed. It blocks that feature only.
func featureBlocked(limit map[string]any) bool {
	if limit == nil {
		return false
	}
	allowed, okA := limit["allowed"].(bool)
	reached, okR := limit["limit_reached"].(bool)
	return (okA && !allowed) || (okR && reached)
}

// codexAllowance is the allowance table, first match wins, so positive
// blocking evidence is never hidden by a malformed sibling field. Codex
// itself treats spend_control.reached and the workspace values of
// rate_limit_reached_type as hard stops, which is why they are account-wide.
func codexAllowance(top, main map[string]any) (AllowanceState, string) {
	spend, _ := top["spend_control"].(map[string]any)
	fields := []struct {
		value   any
		present bool
	}{
		field(main, "allowed"), field(main, "limit_reached"), field(spend, "reached"), field(top, "rate_limit_reached_type"),
	}
	allowed, allowedOK := fields[0].value.(bool)
	reached, reachedOK := fields[1].value.(bool)
	spent, spentOK := fields[2].value.(bool)
	reason, reasonOK := fields[3].value.(string)

	// 1. blocked: any of the four well-typed and positive.
	switch {
	case reasonOK && reason != "":
		return AllowanceBlocked, reason
	case spentOK && spent:
		return AllowanceBlocked, "spend_control.reached"
	case reachedOK && reached:
		return AllowanceBlocked, "rate_limit.limit_reached"
	case allowedOK && !allowed:
		return AllowanceBlocked, "rate_limit.allowed is false"
	}
	// 2. bad: one of them present under a wrong type. Never blocks.
	wellTyped := []bool{allowedOK, reachedOK, spentOK, reasonOK}
	for i, f := range fields {
		if f.present && !wellTyped[i] {
			return AllowanceBad, ""
		}
	}
	if sc, present := top["spend_control"]; present && sc != nil && spend == nil {
		return AllowanceBad, ""
	}
	// 3. allowed, on the vendor's own two words for it.
	if allowedOK && allowed && reachedOK && !reached {
		return AllowanceAllowed, ""
	}
	// 4. unknown — never read as allowed.
	return AllowanceUnknown, ""
}

// field reads one key; a JSON null counts as absent.
func field(m map[string]any, key string) struct {
	value   any
	present bool
} {
	v, ok := m[key]
	return struct {
		value   any
		present bool
	}{v, ok && v != nil}
}
