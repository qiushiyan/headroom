package browser

import (
	"strings"
	"testing"
)

func TestSigninState(t *testing.T) {
	cases := []struct{ url, want string }{
		// The shape `claude auth login` prints, 2.1.309.
		{"https://claude.com/cai/oauth/authorize?code=true&client_id=c&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&code_challenge=x&code_challenge_method=S256&state=Gu7U3GC8UWblcZHKhECIFiHcLekS0jBVAudWdijtO4Y&login_hint=a%40x.com",
			"Gu7U3GC8UWblcZHKhECIFiHcLekS0jBVAudWdijtO4Y"},
		{"https://claude.com/cai/oauth/authorize?client_id=c", ""},
		{"://not a url", ""},
	}
	for _, c := range cases {
		if got := SigninState(c.url); got != c.want {
			t.Errorf("SigninState(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestParseCode(t *testing.T) {
	cases := []struct {
		line string
		want Code
		ok   bool
	}{
		{"abc-_123#Gu7U3GC8", Code{"abc-_123", "Gu7U3GC8"}, true},
		{"abc", Code{}, false},              // no state
		{"#state", Code{}, false},           // no code
		{"abc#", Code{}, false},             // state cut off
		{"abc#de#f", Code{}, false},         // two pastes run together
		{"ab c#def", Code{}, false},         // the paste came apart
		{"\x1b[200~abc#def", Code{}, false}, // a terminal's paste bracket
		{"abc#def\x7f", Code{}, false},      // an erase sent as a byte
		{"abc\u200b#def", Code{}, false},    // a zero-width space from the page
	}
	for _, c := range cases {
		got, ok := ParseCode(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseCode(%q) = %+v,%v; want %+v,%v", c.line, got, ok, c.want, c.ok)
		}
	}
}

// The screen Claude Code 2.1.311 prints on a pipe, measured 2026-10-27: the
// printed URL redirects to the page that shows the code; the one it hands
// $BROWSER, to a localhost listener, and is never the answer.
func TestSigninURL(t *testing.T) {
	const printed = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&code_challenge=x&code_challenge_method=S256&state=hrfl-N8&login_hint=a%40x.com"
	const opened = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A59545%2Fcallback&scope=user%3Ainference&code_challenge=x&code_challenge_method=S256&state=hrfl-N8"
	if u, state, ok := SigninURL("If the browser didn't open, visit: " + printed); !ok || u != printed || state != "hrfl-N8" {
		t.Errorf("printed line: %q %q %v", u, state, ok)
	}
	// With FORCE_HYPERLINK set, as a host file sets it over ssh, the URL comes
	// wrapped in an OSC 8 hyperlink: the escape around it is not part of it.
	linked := "If the browser didn't open, visit: \x1b]8;;" + printed + "\x1b\\" + printed + "\x1b]8;;\x1b\\"
	if u, state, ok := SigninURL(linked); !ok || u != printed || state != "hrfl-N8" {
		t.Errorf("hyperlinked line: %q %q %v", u, state, ok)
	}
	for _, line := range []string{
		"Opening browser to sign in…",
		"Paste code here if prompted > ",
		opened,
		strings.Replace(printed, "&state=hrfl-N8", "", 1),                                   // no state
		strings.Replace(printed, "https%3A%2F%2Fplatform", "http%3A%2F%2Fplatform", 1),      // not https
		strings.Replace(printed, "platform.claude.com", "127.0.0.1%3A4000", 1),              // this machine
		"visit: http://claude.com/cai/oauth/authorize?state=s&redirect_uri=https%3A%2F%2Fx", // not https
	} {
		if u, _, ok := SigninURL(line); ok {
			t.Errorf("%q: found %q", line, u)
		}
	}
}
