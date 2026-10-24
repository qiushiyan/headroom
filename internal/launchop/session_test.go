package launchop

import "testing"

const (
	sessA = "11111111-1111-4111-8111-111111111111"
	sessB = "22222222-2222-4222-8222-222222222222"
)

func TestSessionIntent(t *testing.T) {
	cases := []struct {
		args []string
		want Session
	}{
		{nil, Session{}},
		{[]string{"-p", "hello"}, Session{}},
		{[]string{"--resume", sessA}, Session{Route: sessA, Record: sessA}},
		{[]string{"-r", sessA, "--model", "x"}, Session{Route: sessA, Record: sessA}},
		{[]string{"--resume=" + sessA}, Session{Route: sessA, Record: sessA}},
		{[]string{"--session-id", sessA}, Session{Route: sessA, Record: sessA}},
		{[]string{"--session-id=" + sessA}, Session{Route: sessA, Record: sessA}},
		// A fork follows the session it forks from and creates another: the
		// original is never recorded as having moved.
		{[]string{"--resume", sessA, "--fork-session"}, Session{Route: sessA}},
		{[]string{"--resume", sessA, "--fork-session", "--session-id", sessB}, Session{Route: sessA}},
		{[]string{"--continue"}, Session{Unidentified: true}},
		{[]string{"-c"}, Session{Unidentified: true}},
		{[]string{"--resume"}, Session{Unidentified: true}},
		{[]string{"--resume", "a search term"}, Session{Unidentified: true}},
		{[]string{"--resume="}, Session{}},
		{[]string{"--resume=term"}, Session{Unidentified: true}},
		// Conflicting selectors name nothing headroom can stand behind.
		{[]string{"--continue", "--resume", sessA}, Session{Unidentified: true}},
		// After a bare `--` everything is the vendor's positional text.
		{[]string{"--", "--resume", sessA}, Session{}},
		{[]string{"-p", "--", "--continue"}, Session{}},
		// A value that merely follows the flag is not thereby a session id.
		{[]string{"--session-id", "nope"}, Session{}},
	}
	for _, c := range cases {
		if got := SessionOf(c.args); got != c.want {
			t.Errorf("%v → %+v, want %+v", c.args, got, c.want)
		}
	}
}
