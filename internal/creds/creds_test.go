package creds

import "testing"

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

func TestExpired(t *testing.T) {
	now := int64(1000)
	cases := []struct {
		ms   int64
		want bool
	}{
		{0, false},  // absent → never expires here; the API decides
		{999, true}, // in the past
		{1001, false} /* still valid */}
	for _, c := range cases {
		if got := (Blob{ExpiresAtMS: c.ms}).Expired(now); got != c.want {
			t.Errorf("Expired(%d) = %v, want %v", c.ms, got, c.want)
		}
	}
}
