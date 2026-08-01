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
type Blob struct {
	Token            string
	ExpiresAtMS      int64 // 0 = absent
	SubscriptionType string
	RateLimitTier    string
}

func (b Blob) Expired(nowMS int64) bool {
	return b.ExpiresAtMS > 0 && b.ExpiresAtMS < nowMS
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
	b := Blob{
		Token:            asString(o["accessToken"]),
		ExpiresAtMS:      asInt64(o["expiresAt"]),
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

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return int64(f)
		}
	}
	return 0
}
