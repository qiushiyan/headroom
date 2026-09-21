// Package codexauth is the one reader of Codex's auth.json and of the JWT
// payloads inside it. Discovery, the board, `check` and the 401 re-read all
// go through it, so identity, plan, the access token and its expiry come from
// one file read and cannot disagree.
//
// Observed on codex-cli 0.155.0 (see DESIGN.md § The system observed):
//
//	{"auth_mode":"chatgpt","OPENAI_API_KEY":null,
//	 "tokens":{"id_token":…,"access_token":…,"refresh_token":…,"account_id":…},
//	 "last_refresh":…}
//
// The id token carries `email` and, under "https://api.openai.com/auth",
// `chatgpt_plan_type`, `chatgpt_account_id` and `chatgpt_user_id`. It lives
// one hour and is routinely days expired on a working account, so its expiry
// says nothing about health and is never read. The access token lives about
// ten days; only its `exp` is read. There is no refresh-token expiry field,
// which is why "relogin required" is never produced for Codex: the project
// reads expiry only on positive evidence.
//
// JWT payloads are base64url-decoded and JSON-parsed. Signatures are not
// verified: the file is local, and the vendor is the judge of the token.
package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the credential file inside a Codex home.
const FileName = "auth.json"

// authClaim is the namespaced claim both tokens keep the ChatGPT identity in.
const authClaim = "https://api.openai.com/auth"

// State is where a document lands in the eligibility table. The rows are
// ordered and the first match wins, so positive evidence of one condition is
// never hidden by a later one.
type State int

const (
	// Absent: no auth.json. Never logged in on this home — or logged in with
	// the keyring credential store, which headroom cannot read; `check` is
	// where that difference is reported.
	Absent State = iota
	// Unreadable: the file could not be read, or is not a JSON object.
	Unreadable
	// OtherMode: auth_mode is present and is not "chatgpt" (an API key, an
	// agent identity, Bedrock). Such a login has no subscription window.
	OtherMode
	// NoTokens: a ChatGPT login whose tokens object is absent, not an object,
	// or carries no string access_token.
	NoTokens
	// IdentityUnknown: a usable login whose account cannot be identified —
	// tokens.account_id absent, the id token undecodable, its chatgpt_user_id
	// absent, or account_id disagreeing with the chatgpt_account_id claim.
	IdentityUnknown
	// OK: a ChatGPT login with a complete identity. Whether its access token
	// is still inside its lifetime is TokenStale's question, asked with a
	// clock.
	OK
)

// Snapshot is one read of one auth.json. An account carries the snapshot
// discovery took, and everything a refresh round needs — labels, the ledger
// key, the request's credentials — comes from it. Preparation never reads the
// file again, so a login that changes mid-round cannot put one account's
// response on another's row.
type Snapshot struct {
	State State
	Mode  string // decoded auth_mode; "" when absent (a ChatGPT login: the vendor's struct makes it optional)

	Email     string
	Plan      string // chatgpt_plan_type
	AccountID string // tokens.account_id
	UserID    string // chatgpt_user_id

	AccessToken string
	// ExpiresAt is the access token's `exp`, unix seconds; ExpiresOK is false
	// when the token or the claim could not be decoded.
	ExpiresAt int64
	ExpiresOK bool
}

// Identity is the ledger identity: the account and the person, both required.
// Two homes on one login share a budget and two people in one workspace do
// not. Neither part contains a "/". It is "" unless the snapshot is OK — an
// account that cannot say whose it is gets no ledger key at all.
func (s Snapshot) Identity() string {
	if s.State != OK {
		return ""
	}
	return s.AccountID + "/" + s.UserID
}

// TokenStale reports that the access token cannot be shown to be inside its
// lifetime: its expiry is undecodable, or has passed. Routine, not a login
// problem — any Codex session refreshes it.
func (s Snapshot) TokenStale(nowUnix int64) bool {
	return !s.ExpiresOK || s.ExpiresAt <= nowUnix
}

// Path is the auth document of a Codex home.
func Path(home string) string { return filepath.Join(home, FileName) }

// Read reads a home's auth.json. It never fails: every failure is a State.
func Read(home string) Snapshot {
	data, err := os.ReadFile(Path(home))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Snapshot{State: Absent}
		}
		return Snapshot{State: Unreadable}
	}
	return Parse(data)
}

// Parse applies the eligibility table's document rows (2–5 and 7) to the
// file's bytes. Row 1, the absent file, is Read's; row 6, the stale token,
// needs a clock and is TokenStale's.
func Parse(data []byte) Snapshot {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return Snapshot{State: Unreadable}
	}
	var s Snapshot
	if raw, ok := top["auth_mode"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &s.Mode); err != nil {
			// A mode that is not a string is not "chatgpt".
			s.Mode = string(raw)
		}
		if s.Mode != "chatgpt" {
			s.State = OtherMode
			return s
		}
	}

	var tokens map[string]json.RawMessage
	if raw, ok := top["tokens"]; !ok || json.Unmarshal(raw, &tokens) != nil || tokens == nil {
		s.State = NoTokens
		return s
	}
	if s.AccessToken = str(tokens["access_token"]); s.AccessToken == "" {
		s.State = NoTokens
		return s
	}
	if claims, ok := payload(s.AccessToken); ok {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			s.ExpiresAt, s.ExpiresOK = int64(exp), true
		}
	}

	s.AccountID = str(tokens["account_id"])
	claimAccount := ""
	if claims, ok := payload(str(tokens["id_token"])); ok {
		s.Email, _ = claims["email"].(string)
		if auth, ok := claims[authClaim].(map[string]any); ok {
			s.Plan, _ = auth["chatgpt_plan_type"].(string)
			s.UserID, _ = auth["chatgpt_user_id"].(string)
			claimAccount, _ = auth["chatgpt_account_id"].(string)
		}
	}
	switch {
	case s.AccountID == "", s.UserID == "":
		s.State = IdentityUnknown
	case claimAccount != "" && claimAccount != s.AccountID:
		// The file names one account and the token inside it another. Either
		// could be the truth, so neither is: no budget is keyed on a guess.
		s.State = IdentityUnknown
	case strings.Contains(s.AccountID, "/") || strings.Contains(s.UserID, "/"):
		// The ledger key joins the two with "/"; a part containing one would
		// make two identities spell the same key.
		s.State = IdentityUnknown
	default:
		s.State = OK
	}
	return s
}

func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

// str decodes a JSON string; anything else is "".
func str(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// payload decodes a JWT's claims without verifying its signature.
func payload(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil || claims == nil {
		return nil, false
	}
	return claims, true
}
