package creds

import (
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/tag"
)

func TestServiceName(t *testing.T) {
	if got := ServiceName(""); got != "Claude Code-credentials" {
		t.Errorf("default dir: got %q", got)
	}
	// Known-answer vector computed independently with `shasum -a 256`.
	want := "Claude Code-credentials-05e5d174"
	if got := ServiceName("/Users/qiushi/.claude-accounts/test@example.com"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParse(t *testing.T) {
	blob := `{"claudeAiOauth":{"accessToken":"tok-123","expiresAt":1754121600000,
		"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`
	b, ok := Parse(blob)
	if !ok {
		t.Fatal("expected ok")
	}
	if b.Token != "tok-123" || b.ExpiresAtMS != 1754121600000 {
		t.Errorf("got %+v", b)
	}
	if got := b.PlanLabel(); got != "max 20x" {
		t.Errorf("PlanLabel = %q, want %q", got, "max 20x")
	}

	for name, raw := range map[string]string{
		"not json":         `nope`,
		"no oauth object":  `{"other":{}}`,
		"missing token":    `{"claudeAiOauth":{"expiresAt":1}}`,
		"empty token":      `{"claudeAiOauth":{"accessToken":""}}`,
		"non-string token": `{"claudeAiOauth":{"accessToken":42}}`,
	} {
		if _, ok := Parse(raw); ok {
			t.Errorf("%s: expected !ok", name)
		}
	}

	// Defensive coercions: string expiry, absent plan fields.
	b, ok = Parse(`{"claudeAiOauth":{"accessToken":"t","expiresAt":"123"}}`)
	if !ok || b.ExpiresAtMS != 123 {
		t.Errorf("string expiresAt: got %+v ok=%v", b, ok)
	}
	if got := b.PlanLabel(); got != "?" {
		t.Errorf("absent plan fields: PlanLabel = %q, want ?", got)
	}
	b, _ = Parse(`{"claudeAiOauth":{"accessToken":"t","subscriptionType":"pro"}}`)
	if got := b.PlanLabel(); got != "pro" {
		t.Errorf("subtype fallback: PlanLabel = %q, want pro", got)
	}
	b, _ = Parse(`{"claudeAiOauth":{"accessToken":"t","rateLimitTier":"custom_tier"}}`)
	if got := b.PlanLabel(); got != "custom tier" {
		t.Errorf("unprefixed tier: PlanLabel = %q, want custom tier", got)
	}
}

// The two expiries answer different questions and must never be conflated:
// the access token's passing is routine housekeeping, the refresh token's is
// the only one that means a human has to log in again.
func TestTokenUsable(t *testing.T) {
	now := int64(1000)
	cases := []struct {
		name string
		blob Blob
		want bool
	}{
		{"no token", Blob{}, false},
		{"absent expiry — let the endpoint decide", Blob{Token: "t", ExpiresState: tag.None}, true},
		{"unparseable expiry — don't invent a problem", Blob{Token: "t", ExpiresState: tag.Bad}, true},
		{"aged out", Blob{Token: "t", ExpiresAtMS: 999, ExpiresState: tag.OK}, false},
		{"still valid", Blob{Token: "t", ExpiresAtMS: 1001, ExpiresState: tag.OK}, true},
	}
	for _, c := range cases {
		if got := c.blob.TokenUsable(now); got != c.want {
			t.Errorf("%s: TokenUsable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReloginRequired(t *testing.T) {
	now := int64(1000)
	cases := []struct {
		name string
		blob Blob
		want bool
	}{
		// The regression this whole rework exists for: an access token hours
		// past its expiry, with a refresh token good for weeks, is a healthy
		// account. Claiming otherwise sent the user to /login for nothing.
		{"access aged out, refresh alive", Blob{
			Token: "t", ExpiresAtMS: 1, ExpiresState: tag.OK,
			RefreshExpiresMS: 99999, RefreshState: tag.OK}, false},
		{"refresh genuinely expired", Blob{
			Token: "t", RefreshExpiresMS: 999, RefreshState: tag.OK}, true},
		// Positive evidence only — a field the vendor stopped sending must
		// not be read as an expiry.
		{"refresh absent", Blob{Token: "t", RefreshState: tag.None}, false},
		{"refresh unparseable", Blob{Token: "t", RefreshState: tag.Bad}, false},
	}
	for _, c := range cases {
		if got := c.blob.ReloginRequired(now); got != c.want {
			t.Errorf("%s: ReloginRequired = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseExpiryTags(t *testing.T) {
	b, ok := Parse(`{"claudeAiOauth":{"accessToken":"t","expiresAt":123,"refreshTokenExpiresAt":456}}`)
	if !ok || b.ExpiresAtMS != 123 || b.ExpiresState != tag.OK ||
		b.RefreshExpiresMS != 456 || b.RefreshState != tag.OK {
		t.Errorf("both expiries: %+v ok=%v", b, ok)
	}
	b, _ = Parse(`{"claudeAiOauth":{"accessToken":"t"}}`)
	if b.ExpiresState != tag.None || b.RefreshState != tag.None {
		t.Errorf("absent must tag none, not bad: %+v", b)
	}
	// A malformed timestamp must not degrade to 0, which would read as 1970
	// and therefore as long expired.
	b, _ = Parse(`{"claudeAiOauth":{"accessToken":"t","refreshTokenExpiresAt":{"nested":1}}}`)
	if b.RefreshState != tag.Bad || b.ReloginRequired(time.Now().UnixMilli()) {
		t.Errorf("malformed expiry must be bad and must not force relogin: %+v", b)
	}
}
