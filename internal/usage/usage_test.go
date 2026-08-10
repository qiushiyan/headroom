package usage

import (
	"errors"
	"testing"
	"time"
)

func parseOne(t *testing.T, body string) Row {
	t.Helper()
	rows, err := ParseLimits([]byte(body))
	if err != nil {
		t.Fatalf("ParseLimits(%s): %v", body, err)
	}
	if len(rows) != 1 {
		t.Fatalf("ParseLimits(%s): got %d rows, want 1", body, len(rows))
	}
	return rows[0]
}

func TestParseLimitsModern(t *testing.T) {
	body := `{"limits":[
		{"kind":"session","percent":42.4,"resets_at":"2026-08-02T15:00:00Z","severity":"normal"},
		{"kind":"weekly_all","group":"weekly","percent":"88","resets_at":"2026-08-05T15:00:00.123+00:00"},
		{"kind":"weekly_scoped","group":"weekly","scope":{"model":{"display_name":"Claude Opus 4.5"}},"percent":10,"resets_at":null,"severity":"limit_reached"}
	]}`
	rows, err := ParseLimits([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}

	epoch := time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC).Unix()
	want0 := Row{Label: "5h session", Kind: "session", Percent: 42, ResetAt: epoch, Severity: "normal"}
	if rows[0] != want0 {
		t.Errorf("row0 = %+v, want %+v", rows[0], want0)
	}
	if rows[1].Label != "All models (7d)" || rows[1].Percent != 88 || rows[1].PercentState != StateOK {
		t.Errorf("row1 = %+v", rows[1])
	}
	if rows[1].Group != "weekly" || rows[1].IdentityState != StateOK {
		t.Errorf("row1 identity = %+v", rows[1])
	}
	if rows[1].ResetState != StateOK {
		t.Errorf("fractional+offset timestamp should parse: %+v", rows[1])
	}
	// A model-scoped entry labels by model even when a group is present —
	// and the model travels decoded beside the label.
	if rows[2].Label != "Claude Opus 4.5 (7d)" || rows[2].Model != "Claude Opus 4.5" {
		t.Errorf("row2 = %+v", rows[2])
	}
	if rows[2].ResetAt != 0 || rows[2].ResetState != StateNone {
		t.Errorf("null resets_at should be StateNone: %+v", rows[2])
	}
	if rows[2].Severity != "limit_reached" {
		t.Errorf("severity lost: %+v", rows[2])
	}
}

func TestParseLimitsLegacy(t *testing.T) {
	r := parseOne(t, `{"five_hour":{"utilization":56,"resets_at":"2026-08-02T15:00:00Z"}}`)
	if r.Label != "5h session" || r.Percent != 56 || r.PercentState != StateOK {
		t.Errorf("legacy row = %+v", r)
	}

	// Empty limits array also falls back to legacy.
	r = parseOne(t, `{"limits":[],"five_hour":{"utilization":1}}`)
	if r.Percent != 1 {
		t.Errorf("empty limits should fall back: %+v", r)
	}

	// Missing utilization defaults to 0, well-formed.
	r = parseOne(t, `{"five_hour":{}}`)
	if r.Percent != 0 || r.PercentState != StateOK {
		t.Errorf("legacy default = %+v", r)
	}
}

func TestParseLimitsEmpty(t *testing.T) {
	for _, body := range []string{`{}`, `{"limits":[]}`, `{"five_hour":null}`} {
		rows, err := ParseLimits([]byte(body))
		if err != nil || len(rows) != 0 {
			t.Errorf("ParseLimits(%s) = %v, %v; want no rows, nil", body, rows, err)
		}
	}
}

func TestParseLimitsUnparseable(t *testing.T) {
	for _, body := range []string{`not json`, `[1,2]`, `{"limits":[42]}`, `{"limits":["x"]}`, `{"five_hour":"soon"}`} {
		if _, err := ParseLimits([]byte(body)); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParseLimits(%s): want ErrUnparseable, got %v", body, err)
		}
	}
}

func TestDriftTags(t *testing.T) {
	// Percent present but malformed → renders 0, tagged bad.
	r := parseOne(t, `{"limits":[{"kind":"session","percent":"n/a","resets_at":"2026-08-02T15:00:00Z"}]}`)
	if r.Percent != 0 || r.PercentState != StateBad || !r.Drifted() {
		t.Errorf("bad percent: %+v", r)
	}
	// Percent absent entirely is also drift (the field is required).
	r = parseOne(t, `{"limits":[{"kind":"session"}]}`)
	if r.PercentState != StateBad {
		t.Errorf("missing percent: %+v", r)
	}
	// Timestamp present but unparseable → tagged bad, distinct from absent.
	r = parseOne(t, `{"limits":[{"kind":"session","percent":1,"resets_at":"tomorrowish"}]}`)
	if r.ResetAt != 0 || r.ResetState != StateBad || !r.Drifted() {
		t.Errorf("bad timestamp: %+v", r)
	}
	// Absent / empty timestamps are legitimate (untouched window).
	for _, body := range []string{
		`{"limits":[{"kind":"session","percent":1}]}`,
		`{"limits":[{"kind":"session","percent":1,"resets_at":""}]}`,
	} {
		r = parseOne(t, body)
		if r.ResetState != StateNone || r.Drifted() {
			t.Errorf("absent timestamp should be StateNone: %+v", r)
		}
	}
	// Numeric epochs are accepted (handled drift, not papered over).
	r = parseOne(t, `{"limits":[{"kind":"session","percent":1,"resets_at":1754121600}]}`)
	if r.ResetAt != 1754121600 || r.ResetState != StateOK {
		t.Errorf("numeric epoch: %+v", r)
	}
}

func TestLabelFallbacks(t *testing.T) {
	cases := []struct {
		body, want string
	}{
		{`{"limits":[{"kind":"monthly","percent":1}]}`, "monthly"},
		{`{"limits":[{"group":"monthly","percent":1}]}`, "monthly"},
		{`{"limits":[{"percent":1}]}`, "?"},
		// A scoped row that no longer names its scope must not masquerade as
		// the all-models weekly — a calm number under the wrong identity is
		// the misread this label exists to prevent.
		{`{"limits":[{"kind":"weekly_scoped","group":"weekly","percent":1}]}`, "weekly_scoped"},
	}
	for _, c := range cases {
		if r := parseOne(t, c.body); r.Label != c.want {
			t.Errorf("label for %s = %q, want %q", c.body, r.Label, c.want)
		}
	}
}

func TestIdentityFields(t *testing.T) {
	// The observed vendor vocabulary, decoded verbatim.
	r := parseOne(t, `{"limits":[{"kind":"weekly_scoped","group":"weekly","percent":1,
		"scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}]}`)
	if r.Kind != "weekly_scoped" || r.Group != "weekly" || r.Model != "Fable" ||
		r.IdentityState != StateOK || r.Drifted() {
		t.Errorf("scoped row = %+v", r)
	}

	// The legacy five_hour fallback synthesizes an identifiable session row.
	r = parseOne(t, `{"five_hour":{"utilization":56}}`)
	if r.Kind != "session" || r.IdentityState != StateOK {
		t.Errorf("legacy identity = %+v", r)
	}

	// Identity drift is tagged, never coerced to prose or blanked silently:
	// present-but-untyped fields, a scope that lost its shape, a scoped row
	// with no model name, and a row with no identity at all.
	for _, body := range []string{
		`{"limits":[{"kind":42,"percent":1}]}`,
		`{"limits":[{"kind":"session","group":7,"percent":1}]}`,
		`{"limits":[{"group":"weekly","scope":"model","percent":1}]}`,
		`{"limits":[{"group":"weekly","scope":{"model":"fable"},"percent":1}]}`,
		`{"limits":[{"group":"weekly","scope":{"model":{"display_name":9}},"percent":1}]}`,
		`{"limits":[{"kind":"weekly_scoped","group":"weekly","percent":1}]}`,
		`{"limits":[{"percent":1}]}`,
		// kind is the selector consumers hold; a modern row without it is a
		// row they silently stop matching, whatever else it carries.
		`{"limits":[{"group":"weekly","percent":1}]}`,
		`{"limits":[{"group":"weekly","percent":1,"scope":{"model":{"display_name":"Fable"}}}]}`,
		// The known kinds contradict a model scope: a "5h session" carrying a
		// model, or an all-models weekly scoped to one, is drift wearing a
		// valid selector.
		`{"limits":[{"kind":"session","group":"session","percent":1,"scope":{"model":{"display_name":"Fable"}}}]}`,
		`{"limits":[{"kind":"weekly_all","group":"weekly","percent":1,"scope":{"model":{"display_name":"Fable"}}}]}`,
	} {
		r := parseOne(t, body)
		if r.IdentityState != StateBad || !r.Drifted() {
			t.Errorf("identity for %s = %+v, want StateBad", body, r)
		}
	}

	// A null scope (the unscoped rows' ordinary spelling) is not drift.
	r = parseOne(t, `{"limits":[{"kind":"weekly_all","group":"weekly","percent":1,"scope":null}]}`)
	if r.Model != "" || r.IdentityState != StateOK {
		t.Errorf("null scope = %+v", r)
	}
}
