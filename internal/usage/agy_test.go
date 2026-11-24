package usage

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/tag"
)

// observedAgyAnswer is what `agy -p /usage --output-format json` printed on
// agy 1.5.13 (2026-11-24), descriptions shortened.
const observedAgyAnswer = `{"conversation_id":"","status":"SUCCESS","response":"Gemini Models\tWeekly Limit Remaining\t98%\t2026-11-30T23:07:59Z\n","duration_seconds":0,"num_turns":0,
 "usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0},
 "command":{"name":"usage","data":{"description":"Within each group, models share a weekly limit and a 5-hour limit.",
  "groups":[
   {"name":"Gemini Models","description":"Models within this group: Gemini 4 Neon, …","buckets":[
    {"id":"gemini-weekly","name":"Weekly Limit Remaining","description":"…","window":"weekly","remaining_fraction":0.9813161492347717,"reset_time":"2026-11-30T23:07:59Z"},
    {"id":"gemini-5h","name":"Five Hour Limit Remaining","description":"…","window":"5h","remaining_fraction":0.9972600340843201,"reset_time":"2026-11-25T01:02:02Z"}]},
   {"name":"Claude and GPT models","description":"Models within this group: Claude Opus 5.5, …","buckets":[
    {"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"},
    {"id":"3p-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"}]}]}}}`

func agyBody(email, answer string) []byte {
	return []byte(fmt.Sprintf(`{"email":%q,"agy":%s}`, email, answer))
}

// agyBuckets is an answer holding one group with the given buckets.
func agyBuckets(buckets ...string) string {
	return `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[` +
		strings.Join(buckets, ",") + `]}]}}}`
}

func mustAgy(t *testing.T, body []byte) Reading {
	t.Helper()
	r, err := Parse(config.Agy, body)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, body)
	}
	return r
}

func at(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

func TestAgyObservedAnswer(t *testing.T) {
	r := mustAgy(t, agyBody("me@gmail.com", observedAgyAnswer))
	if r.Email != "me@gmail.com" || r.Allowance.State != AllowanceUnknown || r.AccountID != "" || r.UserID != "" {
		t.Errorf("reading = %+v", r)
	}
	want := []Row{
		{Label: "Gemini 7d", Kind: "gemini-weekly", Group: "weekly", Model: "Gemini Models", WindowSeconds: 604800,
			Percent: 2, ResetAt: at("2026-11-30T23:07:59Z"), Severity: "normal"},
		{Label: "Gemini 5h", Kind: "gemini-5h", Group: "5h", Model: "Gemini Models", WindowSeconds: 18000,
			Percent: 0, ResetAt: at("2026-11-25T01:02:02Z"), Severity: "normal"},
		// Nobody has spent against the group: all of it remains and the
		// reset is the epoch, which is no reset at all.
		{Label: "Claude and GPT 7d", Kind: "3p-weekly", Group: "weekly", Model: "Claude and GPT models", WindowSeconds: 604800,
			Severity: "normal", ResetState: tag.None, Unstarted: true},
		{Label: "Claude and GPT 5h", Kind: "3p-5h", Group: "5h", Model: "Claude and GPT models", WindowSeconds: 18000,
			Severity: "normal", ResetState: tag.None, Unstarted: true},
	}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows = %+v", r.Rows)
	}
	for i := range want {
		if r.Rows[i] != want[i] {
			t.Errorf("row %d = %+v\nwant     %+v", i, r.Rows[i], want[i])
		}
	}
}

// The percent is what is spent: the vendor reports what remains.
func TestAgyPercentIsWhatIsSpent(t *testing.T) {
	for _, c := range []struct {
		remaining string
		pct       int
		state     tag.State
	}{
		{"0", 100, tag.OK},
		{"0.25", 75, tag.OK},
		{"1", 0, tag.OK},
		{"0.996", 0, tag.OK}, // rounds, as every vendor's figure does
		{"1.5", 0, tag.Bad},  // no fraction: drift, never a figure
		{"-0.1", 0, tag.Bad},
		{`"0.5"`, 0, tag.Bad},
		{"null", 0, tag.Bad},
	} {
		r := mustAgy(t, agyBody("", agyBuckets(`{"id":"gemini-5h","window":"5h","remaining_fraction":`+c.remaining+`,"reset_time":"2026-11-25T01:02:02Z"}`)))
		if got := r.Rows[0]; got.Percent != c.pct || got.PercentState != c.state {
			t.Errorf("remaining %s: percent %d %v, want %d %v", c.remaining, got.Percent, got.PercentState, c.pct, c.state)
		}
	}
}

// Unstarted is narrow: all of the window remaining and the epoch as its
// reset. The epoch over a spent window is a reset left out, not a window
// nobody started.
func TestAgyUnstartedNeedsBothSigns(t *testing.T) {
	for _, c := range []struct {
		bucket    string
		unstarted bool
		reset     tag.State
	}{
		{`{"id":"x","window":"5h","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"}`, true, tag.None},
		{`{"id":"x","window":"5h","remaining_fraction":0.5,"reset_time":"1970-01-01T00:00:00Z"}`, false, tag.None},
		{`{"id":"x","window":"5h","remaining_fraction":1,"reset_time":"2026-11-25T01:02:02Z"}`, false, tag.OK},
		{`{"id":"x","window":"5h","remaining_fraction":1}`, false, tag.None},
		{`{"id":"x","window":"5h","remaining_fraction":1,"reset_time":"soon"}`, false, tag.Bad},
	} {
		got := mustAgy(t, agyBody("", agyBuckets(c.bucket))).Rows[0]
		if got.Unstarted != c.unstarted || got.ResetState != c.reset || (got.Unstarted && got.ResetAt != 0) {
			t.Errorf("%s: unstarted %v reset %v at %d, want %v %v", c.bucket, got.Unstarted, got.ResetState, got.ResetAt, c.unstarted, c.reset)
		}
	}
}

// What a bucket cannot say about itself degrades it visibly; nothing is
// dropped.
func TestAgyBucketsDegradeAndAreNeverDropped(t *testing.T) {
	for _, c := range []struct {
		name, answer string
		label        string
	}{
		{"no id", agyBuckets(`{"window":"5h","remaining_fraction":0.5}`), "Gemini 5h"},
		{"an id that is no string", agyBuckets(`{"id":7,"window":"5h","remaining_fraction":0.5}`), "Gemini 5h"},
		{"a bucket that is no object", agyBuckets(`"gemini-5h"`), "Gemini ?"},
		{"a group that is no object", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":["Gemini"]}}}`, "?"},
		{"a group with no bucket list", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models"}]}}}`, "Gemini ?"},
		{"a group name that is no string", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":1,"buckets":[{"id":"x","window":"5h","remaining_fraction":1}]}]}}}`, "5h"},
	} {
		r := mustAgy(t, agyBody("", c.answer))
		if len(r.Rows) != 1 || r.Rows[0].IdentityState != tag.Bad || r.Rows[0].Label != c.label {
			t.Errorf("%s: rows %+v, want one unselectable row labelled %q", c.name, r.Rows, c.label)
		}
	}
	// Two buckets claiming one id are one limit said twice: neither is chosen.
	r := mustAgy(t, agyBody("", agyBuckets(
		`{"id":"gemini-5h","window":"5h","remaining_fraction":0.5}`,
		`{"id":"gemini-5h","window":"5h","remaining_fraction":0.9}`)))
	if !r.Rows[0].Shared || !r.Rows[1].Shared {
		t.Errorf("duplicated ids: %+v", r.Rows)
	}
}

// An unknown window is new vocabulary: carried, labelled, selectable by id.
func TestAgyUnknownWindowIsCarried(t *testing.T) {
	r := mustAgy(t, agyBody("", agyBuckets(`{"id":"gemini-daily","window":"daily","remaining_fraction":0.5}`)))
	got := r.Rows[0]
	if got.Kind != "gemini-daily" || got.Group != "daily" || got.WindowSeconds != 0 || got.Label != "Gemini daily" || got.IdentityState != tag.OK {
		t.Errorf("row = %+v", got)
	}
}

// Only a successful `usage` answer, in headroom's envelope, is this document.
func TestAgyUnparseable(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"not JSON", `broken`},
		{"no answer", `{"email":"me@gmail.com"}`},
		{"a bare answer, not the envelope", observedAgyAnswer},
		{"a failed answer", string(agyBody("", `{"status":"ERROR","command":{"name":"usage","data":{"groups":[]}}}`))},
		{"another command", string(agyBody("", `{"status":"SUCCESS","command":{"name":"credits","data":{"remaining_credits":0}}}`))},
		{"no command", string(agyBody("", `{"status":"SUCCESS","response":"hello"}`))},
		{"no group list", string(agyBody("", `{"status":"SUCCESS","command":{"name":"usage","data":{}}}`))},
		{"a group list that is no list", string(agyBody("", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":{}}}}`))},
	} {
		if _, err := Parse(config.Agy, []byte(c.body)); err != ErrUnparseable {
			t.Errorf("%s: err = %v, want ErrUnparseable", c.name, err)
		}
	}
	// No groups at all is an answer: no limits to report.
	if r := mustAgy(t, agyBody("", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`)); len(r.Rows) != 0 {
		t.Errorf("empty groups: %+v", r.Rows)
	}
}

func TestAgyVocabulary(t *testing.T) {
	rows := mustAgy(t, agyBody("", observedAgyAnswer)).Rows
	// The shortest window, the vendor's first group winning the tie.
	if i := SessionWindow(config.Agy, rows); i != 1 {
		t.Errorf("SessionWindow = %d, want the Gemini 5h row", i)
	}
	for _, r := range rows {
		if !General(config.Agy, r) || Period(config.Agy, r) != 0 {
			t.Errorf("%s: general %v period %v", r.Label, General(config.Agy, r), Period(config.Agy, r))
		}
	}
	// The vendor's order stands.
	if CompareRows(config.Agy, rows[0], rows[3]) != 0 {
		t.Error("CompareRows reorders Antigravity's rows")
	}
}
