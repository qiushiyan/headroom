// Package usage fetches and parses the (undocumented) endpoint Claude
// Code's own /usage screen calls. The response shape has drifted once
// already (legacy five_hour/seven_day giving way to limits[]), so parsing
// is tolerant on purpose — a malformed field degrades to 0 / unknown
// instead of dropping the account — and every degraded field is tagged, so
// `headroom check` can fail on drift the renderer papers over.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/qiushiyan/headroom/internal/tag"
)

// The degradation vocabulary is shared with credential parsing — see
// internal/tag. The aliases keep the usage.State* spelling at call sites.
type FieldState = tag.State

const (
	StateOK   = tag.OK
	StateNone = tag.None
	StateBad  = tag.Bad
)

// RequestSpacing is the quiet period headroom keeps between requests for one
// account. The endpoint's budget is per account and refills in roughly 30-70
// seconds by measurement; this sits deliberately above that, because headroom
// is a bystander on an undocumented endpoint and should err toward silence.
//
// It lives here, with the endpoint it describes, because two packages need it
// and neither may depend on the other: the request ledger spends against it,
// and rendering uses it to decide when an observation is old enough to be
// worth a caption — no newer answer is obtainable inside this window, so
// nagging about age inside it would be nagging about nothing.
const RequestSpacing = 90 * time.Second

// Row is the response contract: one rendered line per limit.
type Row struct {
	Label        string
	Percent      int   // 0 when PercentState is StateBad
	ResetAt      int64 // unix seconds; 0 = unknown or absent
	Severity     string
	PercentState FieldState // StateOK or StateBad
	ResetState   FieldState // StateOK, StateNone, or StateBad
}

// Drifted reports whether any field was present but unparseable.
func (r Row) Drifted() bool {
	return r.PercentState == StateBad || r.ResetState == StateBad
}

// RolledOver reports that the window this row describes has ended: its own
// reset instant is in the past.
//
// The percent then describes a window nobody is spending against any more. It
// is not stale in the ordinary sense — the observation can be seconds old and
// still say this — and it is not drift, because the field parsed. It is simply
// no longer an answer to "how much headroom is left", and a low percent left
// standing reads as headroom that may not exist.
func (r Row) RolledOver(now int64) bool {
	return r.ResetAt != 0 && r.ResetAt <= now
}

var ErrUnparseable = errors.New("response not parseable")

// ParseLimits turns a response body into rows. The limits[] array is
// authoritative; the near-dead legacy five_hour field is the fallback. A
// zero-row nil result with nil error means the account reports no limits.
func ParseLimits(body []byte) ([]Row, error) {
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, ErrUnparseable
	}
	entries, err := limitEntries(top)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, parseEntry(e))
	}
	return rows, nil
}

func limitEntries(top map[string]any) ([]map[string]any, error) {
	if arr, ok := top["limits"].([]any); ok && len(arr) > 0 {
		out := make([]map[string]any, 0, len(arr))
		for _, v := range arr {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, ErrUnparseable
			}
			out = append(out, m)
		}
		return out, nil
	}
	// Legacy shape: a single five_hour object.
	switch fh := top["five_hour"].(type) {
	case nil:
		return nil, nil
	case bool:
		if !fh {
			return nil, nil
		}
		return nil, ErrUnparseable
	case map[string]any:
		pct := fh["utilization"]
		if pct == nil {
			pct = float64(0)
		}
		return []map[string]any{{
			"kind":      "session",
			"percent":   pct,
			"resets_at": fh["resets_at"],
			"severity":  "normal",
		}}, nil
	default:
		return nil, ErrUnparseable
	}
}

func parseEntry(e map[string]any) Row {
	pct, pctState := parsePercent(e["percent"])
	resetAt, resetState := parseReset(e["resets_at"])

	sev := "normal"
	if v, present := e["severity"]; present && v != nil {
		sev = jqString(v)
	}

	model := displayName(e)
	var label string
	switch {
	case strEq(e["kind"], "session"):
		label = "5h session"
	case model != "":
		label = model + " (7d)"
	case strEq(e["group"], "weekly"):
		label = "All models (7d)"
	default:
		if v := e["group"]; v != nil {
			label = jqString(v)
		} else if v := e["kind"]; v != nil {
			label = jqString(v)
		} else {
			label = "?"
		}
	}

	return Row{
		Label:        label,
		Percent:      pct,
		ResetAt:      resetAt,
		Severity:     sev,
		PercentState: pctState,
		ResetState:   resetState,
	}
}

func parsePercent(v any) (int, FieldState) {
	switch n := v.(type) {
	case float64:
		return int(math.Round(n)), StateOK
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return int(math.Round(f)), StateOK
		}
	}
	return 0, StateBad
}

func parseReset(v any) (int64, FieldState) {
	switch t := v.(type) {
	case nil:
		return 0, StateNone
	case string:
		if t == "" {
			return 0, StateNone
		}
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			if sec := ts.Unix(); sec > 0 {
				return sec, StateOK
			}
			return 0, StateNone
		}
		return 0, StateBad
	case float64:
		// Numeric epoch seconds: accepted here (the jq reference marked
		// numbers bad); a drift to numbers is handled, not papered over.
		if sec := int64(t); sec > 0 {
			return sec, StateOK
		}
		return 0, StateNone
	default:
		return 0, StateBad
	}
}

func displayName(e map[string]any) string {
	scope, _ := e["scope"].(map[string]any)
	model, _ := scope["model"].(map[string]any)
	v := model["display_name"]
	if v == nil {
		return ""
	}
	return jqString(v)
}

func strEq(v any, want string) bool {
	s, ok := v.(string)
	return ok && s == want
}

// jqString mirrors jq's tostring for the value kinds the API could send.
func jqString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(s)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

// Result is one account's fetch outcome; Err covers transport failures.
type Result struct {
	StatusCode int
	Body       []byte
	Err        error
}

func Fetch(ctx context.Context, client *http.Client, url, token string) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return Result{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Result{Err: err}
	}
	return Result{StatusCode: resp.StatusCode, Body: body}
}
