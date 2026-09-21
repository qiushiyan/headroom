package codexauth_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/codexauth"
	"github.com/qiushiyan/headroom/internal/codexauth/codexauthtest"
)

func good() codexauthtest.Login {
	return codexauthtest.Login{Email: "a@x.com", Plan: "pro", AccountID: "acct-1", UserID: "user-1", Token: "t1"}
}

// edit rewrites the good document's top level.
func edit(t *testing.T, fn func(top map[string]any)) []byte {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(good().Document(), &top); err != nil {
		t.Fatal(err)
	}
	fn(top)
	b, _ := json.Marshal(top)
	return b
}

// The eligibility table's document rows, at least one document per row, plus
// documents that match two rows to prove the order.
func TestParseEligibilityTable(t *testing.T) {
	tokens := func(top map[string]any) map[string]any { return top["tokens"].(map[string]any) }
	for _, c := range []struct {
		name string
		doc  []byte
		want codexauth.State
	}{
		{"row 2: not JSON", []byte(`not json`), codexauth.Unreadable},
		{"row 2: a JSON array", []byte(`[]`), codexauth.Unreadable},
		{"row 2: JSON null", []byte(`null`), codexauth.Unreadable},
		{"row 3: api key mode", edit(t, func(top map[string]any) { top["auth_mode"] = "apikey" }), codexauth.OtherMode},
		{"row 3: a mode of another type", edit(t, func(top map[string]any) { top["auth_mode"] = 7 }), codexauth.OtherMode},
		{"row 3 before row 4: another mode with no tokens", []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk"}`), codexauth.OtherMode},
		{"row 4: tokens absent", edit(t, func(top map[string]any) { delete(top, "tokens") }), codexauth.NoTokens},
		{"row 4: tokens null", edit(t, func(top map[string]any) { top["tokens"] = nil }), codexauth.NoTokens},
		{"row 4: tokens not an object", edit(t, func(top map[string]any) { top["tokens"] = "x" }), codexauth.NoTokens},
		{"row 4: empty tokens object", edit(t, func(top map[string]any) { top["tokens"] = map[string]any{} }), codexauth.NoTokens},
		{"row 4: access token of another type", edit(t, func(top map[string]any) { tokens(top)["access_token"] = 5 }), codexauth.NoTokens},
		{"row 4 before row 5: no access token and no account id", edit(t, func(top map[string]any) {
			delete(tokens(top), "access_token")
			delete(tokens(top), "account_id")
		}), codexauth.NoTokens},
		{"row 5: account id absent", edit(t, func(top map[string]any) { delete(tokens(top), "account_id") }), codexauth.IdentityUnknown},
		{"row 5: id token undecodable", edit(t, func(top map[string]any) { tokens(top)["id_token"] = "garbage" }), codexauth.IdentityUnknown},
		{"row 5: user id absent", codexauthtest.Login{Email: "a@x.com", AccountID: "acct-1", Token: "t"}.Document(), codexauth.IdentityUnknown},
		{"row 5: account id disagrees with the claim", edit(t, func(top map[string]any) { tokens(top)["account_id"] = "acct-other" }), codexauth.IdentityUnknown},
		{"row 7: a ChatGPT login", good().Document(), codexauth.OK},
		{"row 7: auth_mode absent beside ChatGPT tokens", edit(t, func(top map[string]any) { delete(top, "auth_mode") }), codexauth.OK},
		{"row 7: auth_mode null", edit(t, func(top map[string]any) { top["auth_mode"] = nil }), codexauth.OK},
	} {
		if got := codexauth.Parse(c.doc).State; got != c.want {
			t.Errorf("%s: state = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseDecodesIdentityPlanAndToken(t *testing.T) {
	l := good()
	s := codexauth.Parse(l.Document())
	if s.Email != "a@x.com" || s.Plan != "pro" || s.AccountID != "acct-1" || s.UserID != "user-1" {
		t.Errorf("identity = %+v", s)
	}
	if s.Identity() != "acct-1/user-1" {
		t.Errorf("Identity() = %q", s.Identity())
	}
	if s.AccessToken != l.AccessToken() || !s.ExpiresOK {
		t.Errorf("access token not carried: %+v", s)
	}
	// The id token in the fixture expired in 1970, as a working account's is
	// routinely days expired: it must say nothing about the access token.
	if s.TokenStale(time.Now().Unix()) {
		t.Error("a ten-day access token read as stale")
	}
}

// Row 6: a stale or undecodable access token blocks headroom's request and
// says nothing about the account.
func TestTokenStale(t *testing.T) {
	l := good()
	l.Expires = time.Now().Add(-time.Hour)
	s := codexauth.Parse(l.Document())
	if s.State != codexauth.OK || !s.TokenStale(time.Now().Unix()) {
		t.Errorf("expired access token: state %v stale %v", s.State, s.TokenStale(time.Now().Unix()))
	}
	opaque := edit(t, func(top map[string]any) { top["tokens"].(map[string]any)["access_token"] = "opaque-token" })
	s = codexauth.Parse(opaque)
	if s.State != codexauth.OK || s.ExpiresOK || !s.TokenStale(0) {
		t.Errorf("undecodable expiry: %+v", s)
	}
}

// An account that cannot say whose it is gets no ledger identity at all.
func TestIdentityIsEmptyUnlessOK(t *testing.T) {
	s := codexauth.Parse(codexauthtest.Login{AccountID: "acct-1", Token: "t"}.Document())
	if s.State != codexauth.IdentityUnknown || s.Identity() != "" {
		t.Errorf("state %v identity %q", s.State, s.Identity())
	}
}

func TestReadTellsAbsentFromUnreadable(t *testing.T) {
	home := t.TempDir()
	if s := codexauth.Read(home); s.State != codexauth.Absent {
		t.Errorf("no file: %v, want Absent", s.State)
	}
	if err := os.Mkdir(codexauth.Path(home), 0o755); err != nil { // a directory cannot be read as a file
		t.Fatal(err)
	}
	if s := codexauth.Read(home); s.State != codexauth.Unreadable {
		t.Errorf("unreadable file: %v, want Unreadable", s.State)
	}
	home = t.TempDir()
	good().Write(t, home)
	if s := codexauth.Read(home); s.State != codexauth.OK {
		t.Errorf("good login: %v", s.State)
	}
}
