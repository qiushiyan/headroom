package browser

import "testing"

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
