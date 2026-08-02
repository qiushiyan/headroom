package auth

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	st, ok := Parse([]byte(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty",
	  "email":"a@x.com","orgId":"o","orgName":"O","subscriptionType":"max"}`))
	if !ok || !st.LoggedIn || st.Email != "a@x.com" || st.SubscriptionType != "max" || !st.Answered() {
		t.Errorf("logged-in status: %+v ok=%v", st, ok)
	}

	st, ok = Parse([]byte(`{"loggedIn":false}`))
	if !ok || st.LoggedIn || !st.Answered() {
		t.Errorf("logged-out status: %+v ok=%v", st, ok)
	}
}

// Every failure to get an answer must read as "unknown", never as "logged
// out". An absent Claude Code binary, a changed output shape, or a crash must
// not make four healthy accounts look dead — the caller falls back to
// credential evidence only when Parsed is false.
func TestUnanswerableIsUnknownNotLoggedOut(t *testing.T) {
	for _, raw := range []string{
		``,
		`not json`,
		`{}`,                  // shape drifted: no loggedIn field
		`{"email":"a@x.com"}`, // partial
		`{"loggedIn":"yes"}`,  // wrong type
		`{"loggedIn":null}`,   // explicit null
	} {
		st, ok := Parse([]byte(raw))
		if ok || st.Answered() || st.LoggedIn {
			t.Errorf("%q: got %+v ok=%v, want unparsed and not logged in", raw, st, ok)
		}
	}
}

// The zero Status must mean "no answer". If it meant "answered: logged out",
// every account whose health nobody looked up would render as dead.
func TestZeroStatusIsNoAnswer(t *testing.T) {
	var zero Status
	if zero.Answered() {
		t.Error("an unset Status claims to be a verdict")
	}
	if zero.Outcome != OutcomeUnavailable {
		t.Errorf("zero outcome = %v, want unavailable", zero.Outcome)
	}
}

// Output that ran but no longer parses is drift, and must be distinguishable
// from a command that never ran — one should fail `check`, the other can't.
func TestParseFailureIsUnparseableNotUnavailable(t *testing.T) {
	st, ok := Parse([]byte(`{"totally":"different"}`))
	if ok || st.Outcome != OutcomeUnparseable {
		t.Errorf("drifted output = %+v ok=%v, want unparseable", st, ok)
	}
}

// exec.Cmd.Output returns stdout alongside an ExitError, so a command that
// answered and then exited non-zero has still demonstrably run. Calling that
// "unavailable" would throw away a real verdict — or hide genuine output
// drift behind an inconclusive.
func TestClassifyJudgesOutputBeforeExitStatus(t *testing.T) {
	exitErr := errors.New("exit status 1")
	cases := []struct {
		name string
		out  string
		err  error
		want Outcome
	}{
		{"clean answer", `{"loggedIn":true}`, nil, OutcomeOK},
		{"answer despite non-zero exit", `{"loggedIn":false}`, exitErr, OutcomeOK},
		{"drifted output despite non-zero exit", `{"other":1}`, exitErr, OutcomeUnparseable},
		{"drifted output, clean exit", `{"other":1}`, nil, OutcomeUnparseable},
		{"nothing at all", "", exitErr, OutcomeUnavailable},
		{"silent success is drift", "", nil, OutcomeUnparseable},
	}
	for _, c := range cases {
		if got := classify([]byte(c.out), c.err); got.Outcome != c.want {
			t.Errorf("%s: outcome = %v, want %v", c.name, got.Outcome, c.want)
		}
	}
	if st := classify([]byte(`{"loggedIn":false}`), exitErr); st.LoggedIn {
		t.Error("a logged-out verdict was inverted")
	}
}
