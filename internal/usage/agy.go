package usage

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/agy"
	"github.com/qiushiyan/headroom/internal/tag"
)

// The Antigravity document is headroom's own envelope (agy.Envelope) around
// what `agy -p /usage --output-format json` printed (observed on agy 1.5.13,
// docs/specs/agy-support.md § Antigravity observed):
//
//	{"email":"<the run's sign-in>",
//	 "agy":{"status":"SUCCESS","num_turns":0,…,
//	   "command":{"name":"usage","data":{"groups":[
//	     {"name":"Gemini Models","buckets":[
//	       {"id":"gemini-weekly","window":"weekly","remaining_fraction":0.98,"reset_time":"2026-11-30T23:07:59Z"},
//	       {"id":"gemini-5h","window":"5h","remaining_fraction":0.99,"reset_time":"2026-11-25T01:02:02Z"}]},
//	     {"name":"Claude and GPT models","buckets":[…]}]}}}}
//
// A row's identity is the vendor's own words: Kind is the bucket's id, the
// selector agy gives each limit; Group its window ("5h", "weekly"); Model the
// name of the group whose models share it. The percent is what is spent, as
// every vendor's is here: the vendor reports what remains. Severity is always
// "normal" and the allowance unknown — the document carries neither.

// agyWindows are the window durations agy names. An unknown one is carried
// with no duration and labels itself.
var agyWindows = map[string]int64{
	"5h":     5 * 3600,
	"weekly": 7 * 86400,
}

// parseAgy separates rejecting the document from degrading a field. It is
// unparseable when the envelope or agy's answer is not a JSON object, when the
// answer is not a successful `usage` command, or when it carries no group
// list; everything inside degrades visibly, row by row, and is never dropped.
func parseAgy(body []byte) (Reading, error) {
	var env agy.Envelope
	if err := json.Unmarshal(body, &env); err != nil || len(env.Answer) == 0 {
		return Reading{}, ErrUnparseable
	}
	var answer struct {
		Status  string `json:"status"`
		Command *struct {
			Name string `json:"name"`
			Data *struct {
				Groups []json.RawMessage `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(env.Answer, &answer); err != nil {
		return Reading{}, ErrUnparseable
	}
	// `/quota` answers as `usage` too; anything else is not this document.
	if answer.Status != "SUCCESS" || answer.Command == nil || answer.Command.Name != "usage" ||
		answer.Command.Data == nil || answer.Command.Data.Groups == nil {
		return Reading{}, ErrUnparseable
	}
	r := Reading{Email: env.Email, Rows: []Row{}}
	for _, g := range answer.Command.Data.Groups {
		r.Rows = append(r.Rows, agyGroup(g)...)
	}
	return r, nil
}

// agyGroup is one group's buckets as rows. A group that is not an object, or
// whose bucket list is not a list, is one row that says it cannot be read.
func agyGroup(raw json.RawMessage) []Row {
	var g map[string]any
	if json.Unmarshal(raw, &g) != nil || g == nil {
		return []Row{badIdentityRow("", "?")}
	}
	name, nameOK := identField(g, "name")
	buckets, ok := g["buckets"].([]any)
	if !ok {
		return []Row{badIdentityRow("", agyLabel(name, ""))}
	}
	rows := make([]Row, 0, len(buckets))
	for _, b := range buckets {
		row := agyBucket(b, name)
		if !nameOK {
			row.IdentityState = tag.Bad
		}
		rows = append(rows, row)
	}
	return rows
}

func agyBucket(v any, group string) Row {
	b, ok := v.(map[string]any)
	if !ok {
		return badIdentityRow("", agyLabel(group, ""))
	}
	id, idOK := identField(b, "id")
	window, windowOK := identField(b, "window")
	row := Row{Kind: id, Group: window, Model: group, Severity: "normal", WindowSeconds: agyWindows[window]}
	row.Label = agyLabel(group, window)
	if !idOK || !windowOK || id == "" {
		// The id is what selects a limit; a bucket without one is unselectable.
		row.IdentityState = tag.Bad
	}
	row.Percent, row.PercentState = agySpent(b["remaining_fraction"])
	row.ResetAt, row.ResetState = parseReset(b["reset_time"])
	// A window nobody has spent against reports all of it remaining and a
	// reset at the Unix epoch (seen live on an unused group). Both must hold:
	// the epoch alone over a spent window is a reset the vendor left out.
	if s, _ := b["reset_time"].(string); row.PercentState == tag.OK && row.Percent == 0 && b["remaining_fraction"] == 1.0 {
		if t, err := time.Parse(time.RFC3339, s); err == nil && t.Unix() == 0 {
			row.Unstarted = true
		}
	}
	return row
}

// agySpent is the spent percent of a remaining fraction. Only a number from
// 0 to 1 is a fraction; anything else is drift, never a figure.
func agySpent(v any) (int, tag.State) {
	f, ok := v.(float64)
	if !ok || f < 0 || f > 1 {
		return 0, tag.Bad
	}
	return percentOf((1 - f) * 100)
}

// agyLabel names the models and the window: "Gemini 5h", "Claude and GPT 7d".
// It derives from the decoded fields alone; the group's trailing "Models" is
// left off because every group is one.
func agyLabel(group, window string) string {
	name := strings.TrimSpace(group)
	if trimmed, ok := cutSuffixFold(name, " models"); ok && trimmed != "" {
		name = trimmed
	}
	switch window {
	case "weekly":
		window = "7d"
	case "":
		window = "?"
	}
	if name == "" {
		return window
	}
	return name + " " + window
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s[:len(s)-len(suffix)], true
	}
	return s, false
}
