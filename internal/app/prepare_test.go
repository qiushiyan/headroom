package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
)

// The pipeline's prepare stage, table-tested through the credential seam:
// every per-account credential problem must become that account's status,
// and a healthy account must come out fetch-ready.
func TestPrepareWith(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "primary",
	}

	writeMeta := func(path, email string) {
		t.Helper()
		content := fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q}}`, email)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkAccount := func(name, email string) string {
		t.Helper()
		dir := filepath.Join(cfg.AccountsRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeMeta(filepath.Join(dir, ".claude.json"), email)
		return dir
	}

	writeMeta(cfg.PrimaryMeta(), "primary@x.com")
	goodDir := mkAccount("good@x.com", "good@x.com")
	badDir := mkAccount("bad@x.com", "bad@x.com")
	expiredDir := mkAccount("expired@x.com", "expired@x.com")
	mismatchDir := mkAccount("dir@x.com", "other@x.com")

	goodBlob := `{"claudeAiOauth":{"accessToken":"tok-good","rateLimitTier":"default_claude_max_20x"}}`
	expiredBlob := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-old","expiresAt":%d}}`,
		time.Now().Add(-time.Hour).UnixMilli())
	blobs := map[string]string{
		"":          "", // primary: no credentials anywhere
		goodDir:     goodBlob,
		badDir:      `not json`,
		expiredDir:  expiredBlob,
		mismatchDir: goodBlob,
	}

	if err := accounts.SetCurrent(cfg, "good@x.com"); err != nil {
		t.Fatal(err)
	}

	list, current := prepareWith(cfg, func(configDir string) string { return blobs[configDir] })

	// The returned current target and the per-view Current flags come from
	// one snapshot — consumers (the --json envelope) must never re-read the
	// state file and risk disagreeing with the flags.
	if current != "good@x.com" {
		t.Errorf("current = %q, want good@x.com", current)
	}

	byName := map[string]*accountData{}
	for _, d := range list {
		byName[d.Acct.Name] = d
	}
	if len(list) != 5 {
		t.Fatalf("got %d accounts: %v", len(list), byName)
	}

	if v := byName["primary"].View; v.Status != render.StatusNoLogin || v.Label != "primary@x.com" {
		t.Errorf("primary: %+v", v)
	}
	good := byName["good@x.com"]
	if v := good.View; v.Status != render.StatusPending || !v.Current || v.Plan != "max 20x" {
		t.Errorf("good: %+v", v)
	}
	if !good.NeedsFetch || good.Token != "tok-good" {
		t.Errorf("good not fetch-ready: %+v", good)
	}
	if v := byName["bad@x.com"].View; v.Status != render.StatusBadBlob {
		t.Errorf("bad blob: %+v", v)
	}
	expired := byName["expired@x.com"]
	if v := expired.View; v.Status != render.StatusExpired || expired.NeedsFetch {
		t.Errorf("expired: %+v", v)
	}
	mism := byName["dir@x.com"]
	if v := mism.View; v.Label != "other@x.com" || v.DirMismatch != "dir@x.com" {
		t.Errorf("mismatch not surfaced: %+v", v)
	}
}
