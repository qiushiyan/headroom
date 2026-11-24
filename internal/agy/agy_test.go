package agy

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// The lines are agy 1.5.13's, as a print-mode run writes them to its log.
const signedIn = `I1124 21:18:46.904511       1 printmode.go:409] Print mode: not authenticated, trying silent auth
I1124 21:18:46.919334       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)
I1124 21:18:46.919352       1 server_oauth.go:221] applyAuthResult: email=me@gmail.com, authMethod=consumer, quotaProject=
I1124 21:18:46.919365       1 server_oauth.go:226] OAuth: authenticated successfully as me@gmail.com
`

func TestEmailFromLogIsTheRunsSignIn(t *testing.T) {
	for _, c := range []struct {
		name, log, want string
	}{
		{"the sign-in line", signedIn, "me@gmail.com"},
		{"the last sign-in wins", signedIn + "I1124 21:20:00.000000 1 server_oauth.go:221] applyAuthResult: email=other@gmail.com, authMethod=consumer\n", "other@gmail.com"},
		{"a line with nothing after the email", "x] applyAuthResult: email=end@gmail.com\n", "end@gmail.com"},
		// Another line naming an email is not the sign-in.
		{"no sign-in line", "I1124 1 server_oauth.go:226] OAuth: authenticated successfully as me@gmail.com\n", ""},
		{"an empty log", "", ""},
	} {
		if got := EmailFromLog([]byte(c.log)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWrapKeepsTheAnswerVerbatimBesideWhoseItIs(t *testing.T) {
	answer := `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"remaining_fraction":0.9813161492347717}]}}}`
	body, err := Wrap([]byte("\n"+answer+"\n"), []byte(signedIn))
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Email != "me@gmail.com" || string(env.Answer) != answer {
		t.Errorf("envelope %+v, want the email and the answer byte for byte", env)
	}
}

func TestWrapRefusesWhatIsNoJSONObject(t *testing.T) {
	for _, out := range []string{"", "Error: authentication failed", "[]", "null", `"usage"`} {
		if _, err := Wrap([]byte(out), []byte(signedIn)); !errors.Is(err, ErrNotJSON) {
			t.Errorf("%q: %v, want ErrNotJSON", out, err)
		}
	}
}

func TestArgsAreThePrintModeUsageRead(t *testing.T) {
	want := []string{"-p", "/usage", "--output-format", "json", "--log-file", "/tmp/x.log"}
	if got := Args("/tmp/x.log"); !slices.Equal(got, want) {
		t.Errorf("Args = %q, want %q", got, want)
	}
}
