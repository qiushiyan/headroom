// Package creds reads Claude Code's stored OAuth credentials. Read-only by
// design: tokens are never refreshed or written here — Claude Code owns the
// Keychain. If a token has expired, opening any Claude Code session on that
// account refreshes it.
package creds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiushiyan/headroom/internal/tag"
)

const serviceBase = "Claude Code-credentials"

// ServiceName is the Keychain service Claude Code stores an account's login
// under: the base name for the default ~/.claude, "-"+sha256(dir)[:8] for
// any other config dir. Verified against the binary (v2.1.220); this is the
// load-bearing fact that lets multiple logins coexist.
func ServiceName(configDir string) string {
	if configDir == "" {
		return serviceBase
	}
	sum := sha256.Sum256([]byte(configDir))
	return serviceBase + "-" + hex.EncodeToString(sum[:])[:8]
}

// Blob is the credential contract the whole tool depends on: the fields of
// .claudeAiOauth that rendering and checking need, nothing more.
//
// The two expiries mean very different things and must never be conflated.
// accessToken lives ~8 hours and Claude Code refreshes it silently on its own;
// its passing says nothing about the account, only that *this stored token*
// can't be spent on a usage request right now. refreshToken lives ~30 days and
// is the only one whose passing means a human must run /login again.
type Blob struct {
	Token            string
	ExpiresAtMS      int64 // access token; meaningful only when ExpiresState is OK
	ExpiresState     tag.State
	RefreshExpiresMS int64 // refresh token; meaningful only when RefreshState is OK
	RefreshState     tag.State
	SubscriptionType string
	RateLimitTier    string
}

// TokenUsable reports whether the stored access token can be spent on a live
// usage request. An absent expiry is treated as usable — the endpoint is the
// authority, and refusing to ask on a missing field would invent a problem.
func (b Blob) TokenUsable(nowMS int64) bool {
	if b.Token == "" {
		return false
	}
	return b.ExpiresState != tag.OK || b.ExpiresAtMS > nowMS
}

// ReloginRequired reports the one credential condition a human must act on.
// It demands positive evidence: an absent or unparseable refresh expiry is
// never read as "expired", because guessing wrong here sends the user to
// /login for nothing.
func (b Blob) ReloginRequired(nowMS int64) bool {
	return b.RefreshState == tag.OK && b.RefreshExpiresMS < nowMS
}

// PlanLabel: "default_claude_max_20x" → "max 20x"; falls back to the
// subscription type.
func (b Blob) PlanLabel() string {
	if b.RateLimitTier != "" {
		return strings.ReplaceAll(strings.TrimPrefix(b.RateLimitTier, "default_claude_"), "_", " ")
	}
	return b.SubscriptionType
}

// Parse applies the credential contract to a raw blob. ok is false when the
// blob is not JSON or carries no access token — the "badblob" state, which
// on a real system means Claude Code changed its credential format.
func Parse(raw string) (Blob, bool) {
	var outer struct {
		ClaudeAiOauth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(raw), &outer); err != nil || outer.ClaudeAiOauth == nil {
		return Blob{}, false
	}
	o := outer.ClaudeAiOauth
	expires, expiresState := asEpochMS(o["expiresAt"])
	refresh, refreshState := asEpochMS(o["refreshTokenExpiresAt"])
	b := Blob{
		Token:            asString(o["accessToken"]),
		ExpiresAtMS:      expires,
		ExpiresState:     expiresState,
		RefreshExpiresMS: refresh,
		RefreshState:     refreshState,
		SubscriptionType: asStringDefault(o["subscriptionType"], "?"),
		RateLimitTier:    asString(o["rateLimitTier"]),
	}
	if b.Token == "" {
		return Blob{}, false
	}
	return b, true
}

// ReadKeychain fetches the raw blob from the Keychain item under the
// predicted service name. Empty string = no item (not logged in).
func ReadKeychain(configDir string) string {
	out, err := exec.Command("security", "find-generic-password",
		"-s", ServiceName(configDir), "-a", username(), "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// HasKeychainItem reports whether the item exists at all, without reading
// the secret — the existence assertion `headroom check` makes.
func HasKeychainItem(configDir string) bool {
	err := exec.Command("security", "find-generic-password",
		"-s", ServiceName(configDir), "-a", username()).Run()
	return err == nil
}

// ReadRaw is the rendering path's blob source: Keychain first, then the
// .credentials.json file Claude Code writes where no keychain exists.
func ReadRaw(configDir string) string {
	if blob := ReadKeychain(configDir); blob != "" {
		return blob
	}
	if configDir != "" {
		if data, err := os.ReadFile(filepath.Join(configDir, ".credentials.json")); err == nil {
			return string(data)
		}
	}
	return ""
}

func username() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asStringDefault(v any, def string) string {
	switch s := v.(type) {
	case nil:
		return def
	case bool:
		if !s {
			return def
		}
		return "true"
	case string:
		return s
	default:
		return fmt.Sprint(v)
	}
}

// asEpochMS reads an epoch-milliseconds field, keeping absent distinct from
// unparseable. Only that distinction lets an expiry the vendor stopped
// sending degrade to "unknown" instead of to "1970", which would read as
// long expired.
func asEpochMS(v any) (int64, tag.State) {
	switch n := v.(type) {
	case nil:
		return 0, tag.None
	case float64:
		return int64(n), tag.OK
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, tag.OK
		}
	case string:
		if n == "" {
			return 0, tag.None
		}
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, tag.OK
		}
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return int64(f), tag.OK
		}
	}
	return 0, tag.Bad
}
