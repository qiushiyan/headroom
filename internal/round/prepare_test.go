package round

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/auth"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/state"
)

type prepareFixture struct {
	cfg   config.Scope
	blobs map[string]string
	auth  map[string]auth.Status
	store *state.Store
	// fromFile names the config dirs whose blob comes from the dir's file,
	// not the Keychain, and items what the Keychain shows of each dir's item:
	// absent unless said otherwise.
	fromFile map[string]bool
	items    map[string]creds.Item
	asked    map[string]int // how often the Keychain was asked about each dir's item
}

// read is the fixture's credential read: a blob comes from the Keychain
// unless its dir is in fromFile, and a dir with none reads nothing.
func (f prepareFixture) read(a accounts.Account) creds.Reading {
	raw := f.blobs[a.ConfigDir]
	switch {
	case raw == "":
		return creds.Reading{}
	case f.fromFile[a.ConfigDir]:
		return creds.Reading{Raw: raw, Source: creds.SourceFile}
	}
	return creds.Reading{Raw: raw, Source: creds.SourceKeychain}
}

func (f prepareFixture) run(now time.Time) map[string]*row {
	return byName(f.prepare(now))
}

func (f prepareFixture) prepare(now time.Time) *Round {
	return Prepare(context.Background(), f.cfg, f.store, now, Deps{
		Creds: func(_ context.Context, a accounts.Account) creds.Reading { return f.read(a) },
		Item: func(_ context.Context, a accounts.Account) (creds.Item, string) {
			if f.asked != nil {
				f.asked[a.ConfigDir]++
			}
			if item, ok := f.items[a.ConfigDir]; ok {
				return item, "security: said so"
			}
			return creds.ItemAbsent, ""
		},
		Health: func([]accounts.Account) auth.QueryFunc { return func(dir string) auth.Status { return f.auth[dir] } },
	})
}

// round is prepare followed by the claim, which is what every surface runs and
// where a deferred account is actually decided. Tests about what the user sees
// go through it: prepare alone now marks every spendable account pending,
// because deciding earlier is exactly what let an account skip the claim.
func (f prepareFixture) round(now time.Time) map[string]*row {
	r := f.prepare(now)
	r.Complete(context.Background())
	return byName(r)
}

func byName(r *Round) map[string]*row {
	out := map[string]*row{}
	for _, d := range r.rows {
		out[d.Acct.Name] = d
	}
	return out
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareFileBackedPrimary(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.PrimaryDir(), 0700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com","accountUuid":"primary-id"}}`)
	credential := filepath.Join(cfg.PrimaryDir(), ".credentials.json")
	if err := os.WriteFile(credential, []byte(`{"claudeAiOauth":{"accessToken":"file-token","rateLimitTier":"default_claude_max_20x"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	list := Prepare(context.Background(), cfg, state.Open(cfg), time.Now(), Deps{Creds: func(ctx context.Context, a accounts.Account) creds.Reading {
		return creds.Read(ctx, a.ConfigDir, a.Dir())
	}, Health: ProbeHealth}).rows
	if len(list) == 0 || list[0].View.Plan != "max 20x" || list[0].request == nil {
		t.Fatalf("primary did not get a usable file-backed credential: %+v", list)
	}
}

// The pipeline's prepare stage, table-tested through its three injected
// sources: every per-account problem must become that account's own state,
// and a healthy account must come out fetch-ready.
func TestPrepareWith(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
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
		cfg:   cfg,
		store: state.Open(cfg),
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
	byName := f.run(now)
	if len(byName) != 6 {
		t.Fatalf("got %d accounts: %v", len(byName), byName)
	}

	// The primary's name is never its email (a configured name, or the email's
	// local part), and that is not a mismatch: only an extra's dir, named for
	// one login and holding another, is.
	if v := byName["primary"].View; v.Health != accountstate.HealthNoLogin || v.Label != "primary@x.com" || v.DirMismatch != "" {
		t.Errorf("primary: health=%v label=%q mismatch=%q", v.Health, v.Label, v.DirMismatch)
	}
	good := byName["good@x.com"]
	if v := good.View; v.Health != accountstate.HealthOK || v.Plan != "max 20x" ||
		v.Attempt.State != accountstate.AttemptPending {
		t.Errorf("good: %+v", v)
	}
	if good.request == nil {
		t.Errorf("good not fetch-ready: %+v", good)
	}
	if v := byName["bad@x.com"].View; v.Health != accountstate.HealthBadBlob {
		t.Errorf("bad blob: health=%v", v.Health)
	}

	// The reported bug: a stale access token is a healthy account whose
	// figures merely can't be refreshed by us. It must not be fetched, must
	// not be called expired, and must not be told to log in again.
	stale := byName["stale@x.com"]
	if v := stale.View; v.Health != accountstate.HealthOK || v.Attempt.State != accountstate.AttemptTokenStale {
		t.Errorf("stale access token misclassified: health=%v attempt=%v", v.Health, v.Attempt.State)
	}
	if stale.request != nil {
		t.Error("stale token must not be spent on a request")
	}

	// The genuine case, which still must reach the user.
	if v := byName["gone@x.com"].View; v.Health != accountstate.HealthReloginRequired {
		t.Errorf("dead refresh token: health=%v, want relogin required", v.Health)
	}

	if v := byName["dir@x.com"].View; v.Label != "other@x.com" || v.DirMismatch != "dir@x.com" {
		t.Errorf("mismatch not surfaced: %+v", v)
	}
}

// Health answers one question — can Claude Code use this account — and
// Claude Code's own verdict settles it whenever there is one. A credential
// headroom cannot read blocks *headroom's* request, never the account: it
// belongs on the attempt axis, and reporting it as an account problem is the
// false alarm the three-axis model exists to remove. Credential evidence
// decides only when the oracle has no answer.
//
// And only a usable account is asked about: every row whose health is not OK
// leaves no request and no attempt. Deleting the HealthOK gate in
// claudeAccess must fail the logged-out row — an account Claude Code reports
// logged out, over an access token that has not expired, would otherwise
// claim a ledger slot and spend a request every round, its caption reading
// as fetched over a "/login" health line.
func TestPrepareHealthPrefersAuthStatus(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com"}}`)

	now := time.Now()
	usable := `{"claudeAiOauth":{"accessToken":"t"}}`
	// The refresh token expired long ago, the access token carries no expiry.
	expiredRefresh := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","refreshTokenExpiresAt":%d}}`,
		now.Add(-100*time.Hour).UnixMilli())
	loggedIn := auth.Status{LoggedIn: true, Outcome: auth.OutcomeOK}

	for _, c := range []struct {
		name       string
		oracle     auth.Status
		credential string
		health     accountstate.Health
		attempt    accountstate.AttemptState
		request    bool
	}{
		{"the oracle's logged in outranks an expired refresh token", loggedIn, expiredRefresh,
			accountstate.HealthOK, accountstate.AttemptPending, true},
		{"the oracle's logged out outranks a usable credential, and nothing is asked",
			auth.Status{Outcome: auth.OutcomeOK}, usable, accountstate.HealthNoLogin, accountstate.AttemptNone, false},
		{"an unparseable credential is an attempt fact, not an account one", loggedIn, `not json`,
			accountstate.HealthOK, accountstate.AttemptCredentialUnreadable, false},
		{"a missing credential is an attempt fact, not an account one", loggedIn, "",
			accountstate.HealthOK, accountstate.AttemptCredentialUnreadable, false},
		{"oracle drift is unknown, never papered over with a credential guess",
			auth.Status{Outcome: auth.OutcomeUnparseable}, usable, accountstate.HealthUnknown, accountstate.AttemptNone, false},
		// A probe whose environment the launch seam refused (a relative dir):
		// the credential fallback reads the same broken spelling and would
		// send the user to /login for a path bug.
		{"an unrunnable probe is unknown, never logged out",
			auth.Status{Outcome: auth.OutcomeUnrunnable}, "", accountstate.HealthUnknown, accountstate.AttemptNone, false},
		{"no oracle and no credential is no login", auth.Status{}, "",
			accountstate.HealthNoLogin, accountstate.AttemptNone, false},
		{"no oracle and a credential off the contract is drift", auth.Status{}, `not json`,
			accountstate.HealthBadBlob, accountstate.AttemptNone, false},
	} {
		f := prepareFixture{
			cfg:   cfg,
			store: state.Open(cfg),
			blobs: map[string]string{"": c.credential},
			auth:  map[string]auth.Status{"": c.oracle},
		}
		d := f.run(now)["primary"]
		if d.View.Health != c.health || d.View.Attempt.State != c.attempt || (d.request != nil) != c.request {
			t.Errorf("%s: health %v attempt %v request %v, want %v %v %v", c.name,
				d.View.Health, d.View.Attempt.State, d.request != nil, c.health, c.attempt, c.request)
		}
	}
}

// The bug in one assertion: what headroom fetched moments ago must still read
// as current on the next run, without a request and without a stale nag. The
// account inside its quiet period is deferred, not broken: it says when it
// can next be asked, and its health is untouched.
func TestASecondRunInsideTheQuietPeriodIsStillCurrent(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	if err := os.MkdirAll(cfg.AccountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// A Claude Code cache as old as the one measured on the live machine.
	writeJSON(t, cfg.PrimaryMeta(), fmt.Sprintf(`{
	  "oauthAccount":{"emailAddress":"p@x.com","accountUuid":"uuid-1"},
	  "cachedUsageUtilization":{"fetchedAtMs":%d,"accountUuid":"uuid-1",
	    "utilization":{"limits":[{"kind":"session","percent":58}]}}}`,
		time.Now().Add(-37*time.Hour).UnixMilli()))

	store := state.Open(cfg)
	key := state.Key{UUID: "uuid-1", Name: "primary"}
	fetchedAt := time.Now().Add(-5 * time.Second)
	dec, err := store.Claim([]state.Key{key}, fetchedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(key, dec[0].Generation, state.OutcomeStored,
		[]byte(`{"limits":[{"kind":"session","percent":8}]}`), fetchedAt); err != nil {
		t.Fatal(err)
	}

	f := prepareFixture{
		cfg:   cfg,
		store: store,
		blobs: map[string]string{"": `{"claudeAiOauth":{"accessToken":"t"}}`},
		auth:  map[string]auth.Status{"": {LoggedIn: true, Outcome: auth.OutcomeOK}},
	}
	now := time.Now()
	d := f.round(now)["primary"]

	if d.View.Attempt.State != accountstate.AttemptDeferred {
		t.Errorf("attempt = %v, want deferred", d.View.Attempt.State)
	}
	if d.View.Attempt.NextEligibleAt <= now.Unix() {
		t.Error("a deferred account must say when it becomes eligible")
	}
	if d.View.Health != accountstate.HealthOK {
		t.Errorf("cooling down is not an account problem: health=%v", d.View.Health)
	}
	if !d.View.Fresh(now.Unix()) {
		t.Error("figures fetched five seconds ago read as stale")
	}
	if !d.View.Actionable(now.Unix()) {
		t.Error("the picker would warn against choosing on five-second-old figures")
	}
	block := render.NewPalette(false).Board([]accountstate.Facts{d.View}, now.Unix(), render.LayoutBlocks, 0).Groups[0]
	if len(block) != 1+len(d.View.Obs.Rows) {
		t.Errorf("a nag was printed over current figures: %q", block)
	}
}

// claudeScope is the Claude Code scope under a fixture home, with the
// primary's name pinned the way the fixtures expect it.
func claudeScope(home, primary string) config.Scope {
	s := config.ForHome(home).Claude
	s.PrimaryName = primary
	return s
}

// A bad answer about an account is an answer only when the login its sessions
// use was within reach. An account whose login is a Keychain item this session
// was not given reads logged out to Claude Code itself, and a file beside the
// item is not that login: neither is "not logged in", and neither is asked
// about. The Keychain is consulted only where it changes what is said.
func TestALoginOutOfReachIsNotABadAnswer(t *testing.T) {
	home := t.TempDir()
	cfg := claudeScope(home, "primary")
	writeJSON(t, cfg.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"primary@x.com","accountUuid":"u-primary"}}`)
	dirs := map[string]string{}
	for _, name := range []string{"sealed@x.com", "sealedfile@x.com", "unplaced@x.com", "absent@x.com", "fine@x.com", "expired@x.com"} {
		dir := filepath.Join(cfg.AccountsRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(dir, ".claude.json"), fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q,"accountUuid":"u-%s"}}`, name, name))
		dirs[name] = dir
	}
	now := time.Now()
	good := `{"claudeAiOauth":{"accessToken":"tok"}}`
	dead := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","refreshTokenExpiresAt":%d}}`, now.Add(-time.Hour).UnixMilli())
	loggedOut, loggedIn := auth.Status{Outcome: auth.OutcomeOK}, auth.Status{Outcome: auth.OutcomeOK, LoggedIn: true}
	f := prepareFixture{
		cfg: cfg, store: state.Open(cfg), asked: map[string]int{},
		blobs: map[string]string{
			"":                       good,
			dirs["sealedfile@x.com"]: dead,
			dirs["fine@x.com"]:       good,
			dirs["expired@x.com"]:    dead,
		},
		fromFile: map[string]bool{dirs["sealedfile@x.com"]: true, dirs["fine@x.com"]: true},
		items: map[string]creds.Item{
			dirs["sealed@x.com"]:     creds.ItemPresent,
			dirs["sealedfile@x.com"]: creds.ItemPresent,
			dirs["unplaced@x.com"]:   creds.ItemUnknown,
			dirs["fine@x.com"]:       creds.ItemPresent,
		},
		auth: map[string]auth.Status{
			"":                     loggedIn,
			dirs["sealed@x.com"]:   loggedOut,
			dirs["unplaced@x.com"]: loggedOut,
			dirs["absent@x.com"]:   loggedOut,
			dirs["fine@x.com"]:     loggedIn,
			// sealedfile and expired: the probe has no answer, and the
			// credential read decides.
		},
	}
	rows := f.run(now)
	for name, want := range map[string]struct {
		health accountstate.Health
		login  accountstate.LoginReach
		asked  int
	}{
		"primary":          {accountstate.HealthOK, accountstate.LoginReached, 0},
		"sealed@x.com":     {accountstate.HealthUnknown, accountstate.LoginWithheld, 1},
		"sealedfile@x.com": {accountstate.HealthUnknown, accountstate.LoginWithheld, 1},
		"unplaced@x.com":   {accountstate.HealthUnknown, accountstate.LoginUnplaced, 1},
		"absent@x.com":     {accountstate.HealthNoLogin, accountstate.LoginReached, 1},
		// Claude Code says it is logged in: a file's login it uses is its
		// own business, and nobody is asked about the Keychain.
		"fine@x.com": {accountstate.HealthOK, accountstate.LoginReached, 0},
		// Read from the Keychain itself, so an expired login is an answer.
		"expired@x.com": {accountstate.HealthReloginRequired, accountstate.LoginReached, 0},
	} {
		d := rows[name]
		if d == nil {
			t.Fatalf("no row for %s", name)
		}
		if d.View.Health != want.health || d.View.Login != want.login {
			t.Errorf("%s: health %v, login %v; want %v, %v", name, d.View.Health, d.View.Login, want.health, want.login)
		}
		if got := f.asked[d.Acct.ConfigDir]; got != want.asked {
			t.Errorf("%s: the Keychain was asked %d times, want %d", name, got, want.asked)
		}
		if want.login != accountstate.LoginReached && d.request != nil {
			t.Errorf("%s: a request was prepared on a login out of reach", name)
		}
	}
}
