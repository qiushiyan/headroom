// Package codexauthtest builds Codex auth.json documents for tests, in the
// shape the real writer produces (see package codexauth). The tokens are
// unsigned: headroom never verifies a signature.
package codexauthtest

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Login describes one ChatGPT login. The zero value of a field omits it from
// the document, which is how the degraded rows of the eligibility table are
// built.
type Login struct {
	Email     string
	Plan      string
	AccountID string // tokens.account_id and the chatgpt_account_id claim
	UserID    string
	Token     string    // a marker folded into the access token so tests can tell logins apart
	Expires   time.Time // access token exp; zero means ten days out
}

// JWT encodes claims as an unsigned token.
func JWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

// AccessToken is the access token Document gives this login.
func (l Login) AccessToken() string {
	exp := l.Expires
	if exp.IsZero() {
		exp = time.Now().Add(240 * time.Hour)
	}
	return JWT(map[string]any{"exp": exp.Unix(), "jti": l.Token})
}

// Document is the login's auth.json.
func (l Login) Document() []byte {
	auth := map[string]any{}
	if l.Plan != "" {
		auth["chatgpt_plan_type"] = l.Plan
	}
	if l.AccountID != "" {
		auth["chatgpt_account_id"] = l.AccountID
	}
	if l.UserID != "" {
		auth["chatgpt_user_id"] = l.UserID
	}
	tokens := map[string]any{
		"id_token":      JWT(map[string]any{"email": l.Email, "exp": 1, "https://api.openai.com/auth": auth}),
		"access_token":  l.AccessToken(),
		"refresh_token": "rt-" + l.Token,
	}
	if l.AccountID != "" {
		tokens["account_id"] = l.AccountID
	}
	b, _ := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens":         tokens,
		"last_refresh":   "2026-09-13T08:00:00Z",
	})
	return b
}

// Write puts the login's auth.json into home, creating it.
func (l Login) Write(t testing.TB, home string) {
	t.Helper()
	WriteRaw(t, home, l.Document())
}

// WriteRaw puts arbitrary bytes at home's auth.json.
func WriteRaw(t testing.TB, home string, doc []byte) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
}
