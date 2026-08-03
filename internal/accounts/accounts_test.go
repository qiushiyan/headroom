package accounts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/tag"
	"github.com/qiushiyan/headroom/internal/usage"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	return config.Config{
		Home:         home,
		AccountsRoot: filepath.Join(home, ".claude-accounts"),
		PrimaryName:  "qiushi",
	}
}

func mkAccount(t *testing.T, cfg config.Config, email string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cfg.AccountsRoot, email), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverOrderAndGlob(t *testing.T) {
	cfg := testConfig(t)
	for _, e := range []string{"b@x.com", "a@x.com", "c@x.com"} {
		mkAccount(t, cfg, e)
	}
	// .order promotes c then b (with comments, stray spaces, a missing dir,
	// and a duplicate); a follows alphabetically.
	order := "c@x.com\n# a comment\n  b@x.com  \nmissing@x.com\nc@x.com\n"
	if err := os.WriteFile(cfg.OrderFile(), []byte(order), 0o644); err != nil {
		t.Fatal(err)
	}

	accts := Discover(cfg)
	got := make([]string, len(accts))
	for i, a := range accts {
		got[i] = a.Name
	}
	want := []string{"qiushi", "c@x.com", "b@x.com", "a@x.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if !accts[0].IsPrimary() {
		t.Error("first account should be the primary")
	}
}

func TestDiscoverNoRoot(t *testing.T) {
	cfg := testConfig(t)
	accts := Discover(cfg)
	if len(accts) != 1 || !accts[0].IsPrimary() {
		t.Fatalf("expected only the primary, got %v", accts)
	}
}

func TestLauncher(t *testing.T) {
	cfg := testConfig(t)
	mk := func(name string) Account {
		if name == "" {
			return Account{ConfigDir: "", Name: cfg.PrimaryName}
		}
		return Account{ConfigDir: filepath.Join(cfg.AccountsRoot, name), Name: name}
	}
	// Guaranteed identities only: short local-part aliases are a shell
	// convenience headroom no longer advertises, so there are no collision or
	// reserved-name cases to encode here.
	cases := []struct {
		name string
		want string
	}{
		{"", "x-qiushi"}, // primary → its configured name
		{"yan@planlab.ai", "x-yan@planlab.ai"},
		{"noatsign", "x-noatsign"}, // no @ → the name is the email
	}
	for _, c := range cases {
		if got := Launcher(mk(c.name), cfg.PrimaryName); got != c.want {
			t.Errorf("Launcher(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSelect(t *testing.T) {
	cfg := testConfig(t)
	mkAccount(t, cfg, "yan@planlab.ai")
	accts := Discover(cfg)

	// Absent .current: the documented fresh-start default is the primary.
	a, err := Select(cfg, accts, "")
	if err != nil || !a.IsPrimary() {
		t.Errorf("absent .current: got (%v, %v), want primary", a.Name, err)
	}

	// A recorded, discovered account resolves.
	if err := SetCurrent(cfg, "yan@planlab.ai"); err != nil {
		t.Fatal(err)
	}
	a, err = Select(cfg, accts, "")
	if err != nil || a.Name != "yan@planlab.ai" {
		t.Errorf("recorded account: got (%v, %v)", a.Name, err)
	}

	// An explicit selector bypasses .current.
	a, err = Select(cfg, accts, "qiushi")
	if err != nil || !a.IsPrimary() {
		t.Errorf("explicit primary selector: got (%v, %v)", a.Name, err)
	}

	// Empty .current is corruption, not a choice: fail closed, no primary
	// fallback — the shell used to launch with permissions bypassed on it.
	if err := os.WriteFile(cfg.CurrentFile(), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(cfg, accts, ""); err == nil {
		t.Error("empty .current: want error, got none")
	}

	// A recorded account that no longer exists refuses, naming it.
	if err := SetCurrent(cfg, "gone@x.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(cfg, accts, ""); err == nil {
		t.Error("deleted account in .current: want error, got none")
	}

	// An unknown explicit selector refuses; nothing falls back.
	if _, err := Select(cfg, accts, "nope@x.com"); err == nil {
		t.Error("unknown selector: want error, got none")
	}
}

func TestCurrentTarget(t *testing.T) {
	cfg := testConfig(t)
	if got := CurrentTarget(cfg); got != "qiushi" {
		t.Errorf("no state file: got %q, want primary", got)
	}
	if err := SetCurrent(cfg, "yan@planlab.ai"); err != nil {
		t.Fatal(err)
	}
	if got := CurrentTarget(cfg); got != "yan@planlab.ai" {
		t.Errorf("got %q, want yan@planlab.ai", got)
	}
	if err := os.WriteFile(cfg.CurrentFile(), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CurrentTarget(cfg); got != "qiushi" {
		t.Errorf("empty state file: got %q, want primary", got)
	}
}

func TestSetCurrentAtomicWrite(t *testing.T) {
	cfg := testConfig(t)
	if err := SetCurrent(cfg, "first@x.com"); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrent(cfg, "second@x.com"); err != nil {
		t.Fatal(err)
	}
	if got := CurrentTarget(cfg); got != "second@x.com" {
		t.Errorf("got %q, want second@x.com", got)
	}
	// The write goes through a temp file + rename; nothing may be left over.
	entries, err := os.ReadDir(cfg.AccountsRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".current" {
			t.Errorf("stray file after SetCurrent: %q", e.Name())
		}
	}
}

func TestMetaEmail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	if _, ok := MetaEmail(path); ok {
		t.Error("missing file should not be ok")
	}
	os.WriteFile(path, []byte(`not json`), 0o644)
	if _, ok := MetaEmail(path); ok {
		t.Error("invalid json should not be ok")
	}
	os.WriteFile(path, []byte(`{"oauthAccount":{}}`), 0o644)
	if _, ok := MetaEmail(path); ok {
		t.Error("missing field should not be ok")
	}
	os.WriteFile(path, []byte(`{"oauthAccount":{"emailAddress":"a@b.c"}}`), 0o644)
	email, ok := MetaEmail(path)
	if !ok || email != "a@b.c" {
		t.Errorf("got %q ok=%v, want a@b.c true", email, ok)
	}
}

// The vendor's own cached usage payload must parse through the *same*
// usage.ParseLimits the live endpoint uses. This is the fixture that keeps
// "no second parse path" honest: it is a verbatim excerpt of a real
// .claude.json written by Claude Code 2.1.220.
func TestReadMetaCachedUsageParsesViaSharedParser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	const real = `{
	  "oauthAccount": {"emailAddress":"a@x.com","accountUuid":"9deddc4e-f050-4bb9-a250-e73b68a278e3"},
	  "cachedUsageUtilization": {
	    "fetchedAtMs": 1785622537840,
	    "accountUuid": "9deddc4e-f050-4bb9-a250-e73b68a278e3",
	    "utilization": {
	      "five_hour": {"utilization":0,"resets_at":"2026-08-02T02:49:59.759074+00:00"},
	      "seven_day": {"utilization":16,"resets_at":"2026-08-07T12:59:59.759095+00:00"},
	      "limits": [
	        {"kind":"session","group":"session","percent":0,"severity":"normal",
	         "resets_at":"2026-08-02T02:49:59.759074+00:00","scope":null,"is_active":false},
	        {"kind":"weekly_all","group":"weekly","percent":16,"severity":"normal",
	         "resets_at":"2026-08-07T12:59:59.759095+00:00","scope":null,"is_active":true},
	        {"kind":"weekly_scoped","group":"weekly","percent":29,"severity":"normal",
	         "resets_at":"2026-08-07T12:59:59.759095+00:00",
	         "scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":true}
	      ]}}}`
	if err := os.WriteFile(path, []byte(real), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := ReadMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Email != "a@x.com" || !m.EmailOK {
		t.Errorf("email: %q ok=%v", m.Email, m.EmailOK)
	}
	if m.FetchedAtMS != 1785622537840 {
		t.Errorf("fetchedAtMs: %d", m.FetchedAtMS)
	}

	rows, err := usage.ParseLimits(m.CachedUsage)
	if err != nil {
		t.Fatalf("the shared parser rejected Claude Code's cached payload: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].Label != "5h session" || rows[1].Label != "All models (7d)" ||
		rows[2].Label != "Fable (7d)" {
		t.Errorf("labels: %q %q %q", rows[0].Label, rows[1].Label, rows[2].Label)
	}
	if rows[2].Percent != 29 {
		t.Errorf("percent: %d", rows[2].Percent)
	}
	for i, r := range rows {
		if r.Drifted() {
			t.Errorf("row %d flagged as drift against a real payload: %+v", i, r)
		}
	}
}

// A dir re-logged to another account keeps the old account's cache until
// Claude Code overwrites it. Attributing one account's quota to another is
// worse than showing nothing at all.
func TestReadMetaRejectsCacheFromAnotherAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	const mismatched = `{
	  "oauthAccount": {"emailAddress":"new@x.com","accountUuid":"uuid-new"},
	  "cachedUsageUtilization": {
	    "fetchedAtMs": 1785622537840, "accountUuid": "uuid-previous",
	    "utilization": {"limits":[{"kind":"session","percent":99}]}}}`
	if err := os.WriteFile(path, []byte(mismatched), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.CachedUsage != nil {
		t.Error("cache from a previous login was handed back")
	}
	if m.Email != "new@x.com" {
		t.Errorf("the email is still good and must survive: %q", m.Email)
	}
}

// No cache at all is ordinary — Claude Code only writes one once it has
// fetched. It must not look like an error.
func TestReadMetaWithoutCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"oauthAccount":{"emailAddress":"a@x.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMeta(path)
	if err != nil || m.CachedUsage != nil || m.Email != "a@x.com" {
		t.Errorf("meta without cache: %+v err=%v", m, err)
	}
}

// The identity guard needs both sides present. Accepting the cache when
// either UUID is missing reopens the exact hole it exists to close, and a
// cache with no fetch time would render as observed in 1970.
func TestReadMetaCacheGuardRequiresPositiveEvidence(t *testing.T) {
	cases := []struct {
		name, meta string
	}{
		{"owner uuid missing", `{"oauthAccount":{"emailAddress":"a@x.com"},
		  "cachedUsageUtilization":{"fetchedAtMs":1,"accountUuid":"u",
		    "utilization":{"limits":[{"kind":"session","percent":1}]}}}`},
		{"cache uuid missing", `{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"u"},
		  "cachedUsageUtilization":{"fetchedAtMs":1,
		    "utilization":{"limits":[{"kind":"session","percent":1}]}}}`},
		{"both missing", `{"oauthAccount":{"emailAddress":"a@x.com"},
		  "cachedUsageUtilization":{"fetchedAtMs":1,
		    "utilization":{"limits":[{"kind":"session","percent":1}]}}}`},
		{"no fetch time", `{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"u"},
		  "cachedUsageUtilization":{"accountUuid":"u",
		    "utilization":{"limits":[{"kind":"session","percent":1}]}}}`},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), ".claude.json")
		if err := os.WriteFile(path, []byte(c.meta), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := ReadMeta(path)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if m.CachedUsage != nil {
			t.Errorf("%s: cache accepted without provable provenance", c.name)
		}
		if m.Email != "a@x.com" {
			t.Errorf("%s: the email is independent and must survive", c.name)
		}
	}
}

// A rejected cache and an absent one must be distinguishable, or the day the
// vendor drops accountUuid the fallback disappears silently and check still
// passes.
func TestReadMetaTagsRejectedCacheDistinctlyFromAbsent(t *testing.T) {
	write := func(body string) Meta {
		t.Helper()
		path := filepath.Join(t.TempDir(), ".claude.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := ReadMeta(path)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := write(`{"oauthAccount":{"emailAddress":"a@x.com"}}`); m.CacheState != tag.None {
		t.Errorf("no cache should tag none, got %v", m.CacheState)
	}
	rejected := write(`{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"u"},
	  "cachedUsageUtilization":{"fetchedAtMs":1,"accountUuid":"other",
	    "utilization":{"limits":[{"kind":"session","percent":1}]}}}`)
	if rejected.CacheState != tag.Bad || rejected.CachedUsage != nil {
		t.Errorf("rejected cache: state=%v usable=%v", rejected.CacheState, rejected.CachedUsage != nil)
	}
	good := write(`{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"u"},
	  "cachedUsageUtilization":{"fetchedAtMs":1,"accountUuid":"u",
	    "utilization":{"limits":[{"kind":"session","percent":1}]}}}`)
	if good.CacheState != tag.OK || good.CachedUsage == nil {
		t.Errorf("valid cache: state=%v", good.CacheState)
	}
}
