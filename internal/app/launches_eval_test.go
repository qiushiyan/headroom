package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/eval"
	"github.com/qiushiyan/headroom/internal/launchlog"
	"github.com/qiushiyan/headroom/internal/usage"
	"github.com/qiushiyan/headroom/internal/usagelog"
)

type evalDoc struct {
	Schema  int           `json:"schema"`
	Vendors []eval.Report `json:"vendors"`
}

func runEvalJSON(t *testing.T, scope config.Scope, args ...string) evalDoc {
	t.Helper()
	var out bytes.Buffer
	if code := runLaunchesTo(&out, []config.Scope{scope}, append([]string{"--eval", "--json"}, args...)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc evalDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if doc.Schema != EvalSchema || len(doc.Vendors) != 1 {
		t.Fatalf("document = %+v", doc)
	}
	return doc
}

// A launch logs what the rule was asked and every account's key, and the
// evaluation joins that line to the readings that followed it.
func TestTheEvaluationJoinsALaunchToWhatFollowed(t *testing.T) {
	f := newAutoFixture(t, "a@x.com")
	f.setCurrent("auto\n")
	f.observe("qiushi", 40, 40, time.Minute)
	f.observe("a@x.com", 5, 10, time.Minute)
	if got := f.launch(); got.code != 0 {
		t.Fatal(got.stderr)
	}
	f.launch("--account", "qiushi")

	recs := f.log()
	if r := recs[0]; r.V != launchlog.Version || !r.Automatic || loggedCandidate(r, "a@x.com").Key != "uuid:u-a@x.com" {
		t.Fatalf("launch line = v%d automatic %v candidates %+v", r.V, r.Automatic, r.Candidates)
	}
	if recs[1].Automatic {
		t.Error("a named launch logged as the rule's choice")
	}

	// What the refresh after the launch would have logged.
	ok := usage.StateOK
	later := time.Now().Add(time.Minute)
	if err := usagelog.Append(f.cfg.AccountsRoot, usagelog.New(later, config.Claude, "uuid:u-a@x.com", "a@x.com", f.cfg.AccountsRoot,
		[]usage.Row{{Kind: "session", Label: "5h session", Percent: 30, ResetAt: later.Add(time.Hour).Unix(), PercentState: ok, ResetState: ok, IdentityState: ok}},
		usage.Allowance{})); err != nil {
		t.Fatal(err)
	}

	rep := runEvalJSON(t, f.cfg).Vendors[0]
	if rep.Sources.Launches != 2 || rep.Sources.Logged != 1 || rep.Sources.Homes != 1 || len(rep.Decisions) != 1 {
		t.Fatalf("sources %+v, %d decisions", rep.Sources, len(rep.Decisions))
	}
	d := rep.Decisions[0]
	if d.Chosen != "a@x.com" || !d.Replay.Agrees || !d.Replay.Exact {
		t.Errorf("decision = %+v", d)
	}
	if d.ChosenAfter == nil || d.ChosenAfter.Counted != 5 || d.ChosenAfter.Peak != 30 {
		t.Errorf("after = %+v", d.ChosenAfter)
	}

	var out bytes.Buffer
	if code := runLaunchesTo(&out, []config.Scope{f.cfg}, []string{"--eval"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	text := out.String()
	for _, want := range []string{"claude placement", "read 2 launch line(s) from 1 home(s), 1 usage-log reading(s)", "replay agrees", "1 of 1", "a@x.com"} {
		if !strings.Contains(text, want) {
			t.Errorf("text report lacks %q:\n%s", want, text)
		}
	}
}

// The evaluation reads every home that spends against the ledger: a second
// home's launches are its decisions too, said as that home's.
func TestTheEvaluationReadsEveryHomeOnTheLedger(t *testing.T) {
	f := newTwoHomes(t)
	f.observe("a@x.com", 10, 10)
	f.observe("b@x.com", 20, 10)
	f.observe("yan@x.com", 30, 10)
	if got := f.launch(f.steward, "--auto"); got.code != 0 {
		t.Fatal(got.stderr)
	}
	if got := f.launch(f.owner, "--auto"); got.code != 0 {
		t.Fatal(got.stderr)
	}
	rep := runEvalJSON(t, f.owner).Vendors[0]
	if rep.Sources.Homes != 2 || len(rep.Decisions) != 2 || rep.Sources.Unkeyed != 0 {
		t.Fatalf("sources %+v, decisions %+v", rep.Sources, rep.Decisions)
	}
	homes := map[string]bool{}
	for _, d := range rep.Decisions {
		homes[d.Home] = true
		if !d.Replay.Agrees {
			t.Errorf("replay of %s's launch: %+v", d.Home, d.Replay)
		}
	}
	if !homes[f.steward.AccountsRoot] || !homes[f.owner.AccountsRoot] {
		t.Errorf("decision homes = %v", homes)
	}
}

func TestTheEvaluationsArguments(t *testing.T) {
	f := newAutoFixture(t)
	for _, args := range [][]string{
		{"--eval", "-n", "3"},
		{"--since", "7d"},
		{"--eval", "--since"},
		{"--eval", "--since", "soon"},
	} {
		var out bytes.Buffer
		stderr := captureStderr(t, func() {
			if code := runLaunchesTo(&out, []config.Scope{f.cfg}, args); code != 2 {
				t.Errorf("%v: exit %d, want 2", args, code)
			}
		})
		if stderr == "" {
			t.Errorf("%v: refused without saying why", args)
		}
	}
	var out bytes.Buffer
	if code := runLaunchesTo(&out, []config.Scope{f.cfg}, []string{"--eval"}); code != 0 || !strings.Contains(out.String(), "nothing recorded yet") {
		t.Errorf("an empty log: exit %d\n%s", code, out.String())
	}
	if doc := runEvalJSON(t, f.cfg, "--since", "2026-10-01"); doc.Vendors[0].Since == nil {
		t.Error("--since did not reach the report")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"36h":                  now.Add(-36 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"2026-10-01T08:00:00Z": time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC),
		"2026-10-01":           time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "-3d", "3w"} {
		if _, err := parseSince(in, now); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}
