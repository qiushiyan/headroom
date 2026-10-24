package browser

import (
	"net/url"
	"strings"
)

// SigninEnv carries a file path from the command that runs a vendor with
// headroom as its $BROWSER to the headroom process the vendor runs, which
// writes there the state of the sign-in the URL it was handed belongs to.
// Present without ProfileEnv, it makes headroom a recorder that opens
// nothing.
const SigninEnv = "HEADROOM_BROWSER_SIGNIN"

// SigninState is the state parameter of a vendor authorize URL: the value
// the code its page shows must carry. Claude Code builds the URL it opens and
// the URL it prints from one state (read from 2.1.309), so either names the
// sign-in. "" when the URL carries none.
func SigninState(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}

// Code is a sign-in code as the vendor's callback page shows it.
type Code struct {
	Code, State string
}

// ParseCode reads a pasted line as the vendor's `code#state`: both parts
// present, and nothing a copy of the page could have added. An OAuth code
// and state are printable ASCII (RFC 6749's VSCHAR), so a control character
// or anything past ASCII came from the terminal or the page around the
// code, and a space inside the line means the paste came apart. The line
// arrives already trimmed.
func ParseCode(line string) (Code, bool) {
	if strings.ContainsFunc(line, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
		return Code{}, false
	}
	code, state, ok := strings.Cut(line, "#")
	if !ok || code == "" || state == "" || strings.Contains(state, "#") {
		return Code{}, false
	}
	return Code{Code: code, State: state}, true
}
