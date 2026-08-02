package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/throttle"
)

type prepareFixture struct {
	cfg   config.Config
	blobs map[string]string
	auth  map[string]auth.Status
	th    *throttle.Store
}

func (f prepareFixture) run(now time.Time) map[string]*accountData {
	list, _ := prepareWith(f.cfg, accounts.Discover(f.cfg), f.th, sources{
		readRaw: func(dir string) string { return f.blobs[dir] },
		health:  func(dir string) auth.Status { return f.auth[dir] },
		now:     now,
	})
	byName := map[string]*accountData{}
	for _, d := range list {
		byName[d.Acct.Name] = d
	}
	return byName
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The pipeline's prepare stage, table-tested through its three injected
// sources: every per-account problem must become that account's own state,
// and a healthy account must come out fetch-ready.
func TestPrepareWith(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "primary",
	}
	mkAccount := func(name, email string) string {
		t.Helper()
		dir := filepath.Join(cfg.AccountsRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(dir, ".claude.json"),
			fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q}}`, email))
		return dir
	}

	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com"}}`)
	goodDir := mkAccount("good@x.com", "good@x.com")
	badDir := mkAccount("bad@x.com", "bad@x.com")
	staleDir := mkAccount("stale@x.com", "stale@x.com")
	goneDir := mkAccount("gone@x.com", "gone@x.com")
	mismatchDir := mkAccount("dir@x.com", "other@x.com")

	now := time.Now()
	goodBlob := `{"claudeAiOauth":{"accessToken":"tok-good","rateLimitTier":"default_claude_max_20x"}}`
	f := prepareFixture{
		cfg: cfg,
		th:  throttle.Load(cfg.AccountsRoot),
		blobs: map[string]string{
			"":      "", // primary: no credentials anywhere
			goodDir: goodBlob,
			badDir:  `not json`,
			// Access token hours past expiry, refresh token good for weeks.
			staleDir: fmt.Sprintf(
				`{"claudeAiOauth":{"accessToken":"tok-old","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
				now.Add(-time.Hour).UnixMilli(), now.Add(700*time.Hour).UnixMilli()),
			// Refresh token itself expired — the one case a human must fix.
			goneDir: fmt.Sprintf(
				`{"claudeAiOauth":{"accessToken":"tok-dead","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
				now.Add(-time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli()),
			mismatchDir: goodBlob,
		},
		auth: map[string]auth.Status{}, // no Claude Code install: fall back to creds
	}
	if err := accounts.SetCurrent(cfg, "good@x.com"); err != nil {
		t.Fatal(err)
	}

	byName := f.run(now)
	if len(byName) != 6 {
		t.Fatalf("got %d accounts: %v", len(byName), byName)
	}

	if v := byName["primary"].View; v.Health != render.HealthNoLogin || v.Label != "primary@x.com" {
		t.Errorf("primary: health=%v label=%q", v.Health, v.Label)
	}
	good := byName["good@x.com"]
	if v := good.View; v.Health != render.HealthOK || !v.Current || v.Plan != "max 20x" ||
		v.Attempt.State != render.AttemptPending {
		t.Errorf("good: %+v", v)
	}
	if !good.NeedsFetch || good.Token != "tok-good" {
		t.Errorf("good not fetch-ready: %+v", good)
	}
	if v := byName["bad@x.com"].View; v.Health != render.HealthBadBlob {
		t.Errorf("bad blob: health=%v", v.Health)
	}

	// The reported bug: a stale access token is a healthy account whose
	// figures merely can't be refreshed by us. It must not be fetched, must
	// not be called expired, and must not be told to log in again.
	stale := byName["stale@x.com"]
	if v := stale.View; v.Health != render.HealthOK || v.Attempt.State != render.AttemptTokenStale {
		t.Errorf("stale access token misclassified: health=%v attempt=%v", v.Health, v.Attempt.State)
	}
	if stale.NeedsFetch {
		t.Error("stale token must not be spent on a request")
	}

	// The genuine case, which still must reach the user.
	if v := byName["gone@x.com"].View; v.Health != render.HealthReloginRequired {
		t.Errorf("dead refresh token: health=%v, want relogin required", v.Health)
	}

	if v := byName["dir@x.com"].View; v.Label != "other@x.com" || v.DirMismatch != "dir@x.com" {
		t.Errorf("mismatch not surfaced: %+v", v)
	}
}

// Claude Code's own verdict decides identity; a credential that fails the
// contract still outranks it, because that is drift worth seeing.
func TestPrepareHealthPrefersAuthStatus(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com"}}`)

	now := time.Now()
	// A blob whose refresh token expired long ago, but Claude Code says the
	// account is logged in — the first-party answer wins.
	blob := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","refreshTokenExpiresAt":%d}}`,
		now.Add(-100*time.Hour).UnixMilli())
	f := prepareFixture{
		cfg:   cfg,
		th:    throttle.Load(cfg.AccountsRoot),
		blobs: map[string]string{"": blob},
		auth:  map[string]auth.Status{"": {LoggedIn: true, Outcome: auth.OutcomeOK}},
	}
	if v := f.run(now)["primary"].View; v.Health != render.HealthOK {
		t.Errorf("auth status ignored: health=%v", v.Health)
	}

	f.auth = map[string]auth.Status{"": {LoggedIn: false, Outcome: auth.OutcomeOK}}
	if v := f.run(now)["primary"].View; v.Health != render.HealthNoLogin {
		t.Errorf("logged-out account not surfaced: health=%v", v.Health)
	}

	// An unreadable credential blocks headroom's request but is not an
	// account-health verdict — the first-party oracle still decides that.
	f.auth = map[string]auth.Status{"": {LoggedIn: true, Outcome: auth.OutcomeOK}}
	f.blobs = map[string]string{"": `not json`}
	if v := f.run(now)["primary"].View; v.Health != render.HealthOK ||
		v.Attempt.State != render.AttemptCredentialUnreadable {
		t.Errorf("unreadable credential mishandled: health=%v attempt=%v", v.Health, v.Attempt.State)
	}

	// The oracle answering in a shape we no longer parse is drift, and must
	// not be papered over with a credential guess.
	f.auth = map[string]auth.Status{"": {Outcome: auth.OutcomeUnparseable}}
	f.blobs = map[string]string{"": `{"claudeAiOauth":{"accessToken":"t"}}`}
	if v := f.run(now)["primary"].View; v.Health != render.HealthUnknown {
		t.Errorf("auth output drift not surfaced: health=%v", v.Health)
	}
}

// Claude Code's own cache seeds the display for free, so a refused or deferred
// refresh still leaves the user with numbers — labelled with their real age.
func TestPrepareSeedsFromClaudeCache(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fetchedAt := time.Now().Add(-22 * time.Hour)
	writeJSON(t, cfg.PrimaryMeta(), fmt.Sprintf(`{
	  "oauthAccount":{"emailAddress":"primary@x.com","accountUuid":"uuid-1"},
	  "cachedUsageUtilization":{
	    "fetchedAtMs":%d,"accountUuid":"uuid-1",
	    "utilization":{"limits":[{"kind":"session","percent":58,"severity":"normal",
	      "resets_at":"2026-08-06T17:00:00Z"}]}}}`, fetchedAt.UnixMilli()))

	f := prepareFixture{
		cfg:   cfg,
		th:    throttle.Load(cfg.AccountsRoot),
		blobs: map[string]string{"": `{"claudeAiOauth":{"accessToken":"t"}}`},
		auth:  map[string]auth.Status{"": {LoggedIn: true, Outcome: auth.OutcomeOK}},
	}
	now := time.Now()
	v := f.run(now)["primary"].View
	if v.Obs == nil {
		t.Fatal("cached usage not loaded")
	}
	if v.Obs.Source != render.SourceCache || v.Obs.Rows[0].Percent != 58 {
		t.Errorf("cache mis-parsed: %+v", v.Obs)
	}
	if v.Fresh(now.Unix()) {
		t.Error("a 22h-old cache must not count as current headroom")
	}
}

// A config dir re-logged to a different account keeps the previous account's
// cache. Rendering it under the new name would attribute one account's quota
// to another — worse than showing nothing.
func TestPrepareRejectsCacheFromAnotherAccount(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), fmt.Sprintf(`{
	  "oauthAccount":{"emailAddress":"new@x.com","accountUuid":"uuid-new"},
	  "cachedUsageUtilization":{
	    "fetchedAtMs":%d,"accountUuid":"uuid-previous",
	    "utilization":{"limits":[{"kind":"session","percent":99}]}}}`,
		time.Now().UnixMilli()))

	f := prepareFixture{
		cfg:   cfg,
		th:    throttle.Load(cfg.AccountsRoot),
		blobs: map[string]string{"": `{"claudeAiOauth":{"accessToken":"t"}}`},
		auth:  map[string]auth.Status{"": {LoggedIn: true, Outcome: auth.OutcomeOK}},
	}
	if v := f.run(time.Now())["primary"].View; v.Obs != nil {
		t.Errorf("cache belonging to a previous login was used: %+v", v.Obs)
	}
}

// An account inside its quiet period is not fetched, and says so rather than
// looking broken.
func TestPrepareDefersInsideQuietPeriod(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com"}}`)

	now := time.Now()
	th := throttle.Load(cfg.AccountsRoot)
	th.NoteRefused("primary", now)

	f := prepareFixture{
		cfg:   cfg,
		th:    th,
		blobs: map[string]string{"": `{"claudeAiOauth":{"accessToken":"t"}}`},
		auth:  map[string]auth.Status{"": {LoggedIn: true, Outcome: auth.OutcomeOK}},
	}
	d := f.run(now)["primary"]
	if d.NeedsFetch {
		t.Error("fetched an account inside its cooldown")
	}
	if d.View.Attempt.State != render.AttemptDeferred {
		t.Errorf("attempt = %v, want deferred", d.View.Attempt.State)
	}
	if d.View.Attempt.NextEligibleAt <= now.Unix() {
		t.Error("deferred account must say when it becomes eligible")
	}
	if d.View.Health != render.HealthOK {
		t.Errorf("cooling down is not an account problem: health=%v", d.View.Health)
	}
}

// A cache saying "no limit windows" is an answer, not an absence — the same
// answer the live endpoint is allowed to give. Dropping it left the user with
// "usage unknown" while a perfectly good cached fact sat on disk.
func TestPrepareKeepsZeroRowCache(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home, AccountsRoot: filepath.Join(home, ".claude-accounts"), PrimaryName: "primary"}
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), fmt.Sprintf(`{
	  "oauthAccount":{"emailAddress":"p@x.com","accountUuid":"u"},
	  "cachedUsageUtilization":{"fetchedAtMs":%d,"accountUuid":"u",
	    "utilization":{"limits":[]}}}`, time.Now().UnixMilli()))

	f := prepareFixture{
		cfg:   cfg,
		th:    throttle.Load(cfg.AccountsRoot),
		blobs: map[string]string{"": `{"claudeAiOauth":{"accessToken":"t"}}`},
		auth:  map[string]auth.Status{"": {LoggedIn: true, Outcome: auth.OutcomeOK}},
	}
	v := f.run(time.Now())["primary"].View
	if v.Obs == nil {
		t.Fatal("a zero-row cache was discarded instead of shown as 'no limits'")
	}
	if len(v.Obs.Rows) != 0 || v.Obs.Source != render.SourceCache {
		t.Errorf("observation: %+v", v.Obs)
	}
}
