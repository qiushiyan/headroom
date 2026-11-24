package browser

import (
	"net/url"
	"regexp"
	"strings"
)

// SigninEnv carries a file path from the command that runs a vendor with
// headroom as its $BROWSER to the headroom process the vendor runs, which
// writes there the state of the sign-in the URL it was handed belongs to.
// Present without ProfileEnv, it makes headroom a recorder that opens
// nothing.
const SigninEnv = "HEADROOM_BROWSER_SIGNIN"

// SigninTags are the values that name the sign-in of an authorize URL on
// the page that shows its code, `code#<tag>`: its PKCE code_challenge and its
// state, challenge first. Claude Code's printed URL is the short link
// `…/oauth/authorize?z-<challenge>` (measured 2.1.331), whose page's code
// carries the challenge; while it printed the full URL (2.1.309–2.1.311), the
// code carried the state the URL it opens shares. Each is drawn afresh for a
// sign-in, so either names it. None when the URL carries neither.
func SigninTags(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	var tags []string
	for _, key := range []string{"code_challenge", "state"} {
		if v := u.Query().Get(key); v != "" {
			tags = append(tags, v)
		}
	}
	return tags
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

// SigninURL finds, in one line of the vendor's login screen, the sign-in URL
// a browser on another machine can complete, and the tag its page's code
// carries (SigninTags). It is either Claude Code's short link, an https
// `…/oauth/authorize?z-<challenge>` off this machine (2.1.331: `If the browser
// didn't open, visit: <URL>`), or a full authorize URL carrying a state and an
// https redirect_uri off this machine — the page that shows the code to paste
// (2.1.311, redirecting to platform.claude.com/oauth/code/callback). The URL
// Claude Code hands its $BROWSER redirects to a localhost listener instead,
// and is never it. headroom hands out only a URL the vendor printed, never
// one it built.
func SigninURL(line string) (signinURL, tag string, ok bool) {
	for field := range strings.FieldsSeq(terminalEscape.ReplaceAllString(line, " ")) {
		if !strings.HasPrefix(field, "https://") {
			continue
		}
		u, err := url.Parse(field)
		if err != nil || localHost(u.Hostname()) {
			continue
		}
		if short, found := strings.CutPrefix(u.RawQuery, "z-"); found && strings.HasSuffix(u.Path, "/oauth/authorize") {
			if challenge, _, _ := strings.Cut(short, "&"); challenge != "" {
				return field, challenge, true
			}
			continue
		}
		q := u.Query()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if q.Get("state") == "" || err != nil || redirect.Scheme != "https" || localHost(redirect.Hostname()) {
			continue
		}
		return field, q.Get("state"), true
	}
	return "", "", false
}

// terminalEscape matches what a terminal reads and never shows: an OSC
// sequence (an OSC 8 hyperlink wraps the URL when FORCE_HYPERLINK is set, as a
// host file sets it over ssh) and a CSI sequence such as a colour.
var terminalEscape = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]`)

func localHost(host string) bool {
	return host == "" || host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1"
}
