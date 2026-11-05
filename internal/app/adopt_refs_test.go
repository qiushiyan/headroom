package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/headless"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

// legacyHome is a home a build before refs wrote: the primary logged in as
// email, one extra (a@x.com), and state.json holding doc's sections as that
// build spelled them.
func legacyHome(t *testing.T, email string, doc map[string]any) config.Config {
	t.Helper()
	cfg := config.ForHome(t.TempDir())
	extra := filepath.Join(cfg.Claude.AccountsRoot, "a@x.com")
	for _, dir := range []string{cfg.Claude.PrimaryDir(), extra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(t, cfg.Claude.PrimaryMeta(), `{"oauthAccount":{"emailAddress":"`+email+`"}}`)
	writeJSON(t, filepath.Join(extra, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@x.com"}}`)
	doc["version"] = 1
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Claude.AccountsRoot, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func legacyRehome(account string) map[string]any {
	return map[string]any{"account": account, "atMs": 1}
}

func ownerOf(t *testing.T, scope config.Scope, id string) string {
	t.Helper()
	set := accounts.Discover(scope)
	owner, _ := sessions.OwnerOf(id, launchop.Refs(set), sessions.ProcessEvidence{}, state.Open(scope).Load().Owners())
	return owner
}

// A home whose build recorded the primary's name — and the names it had
// before — moves every record under any of them to the ref, once, and leaves
// the extra's alone: the shape of a machine that pinned its primary by email
// after a local part. The records then follow the primary under any name.
func TestARecordedPrimaryNameIsAdoptedOnce(t *testing.T) {
	cfg := legacyHome(t, "me@gmail.com", map[string]any{
		"primary":  map[string]any{"name": "me@gmail.com", "former": []string{"old-pin"}},
		"sessions": map[string]any{"s-now": legacyRehome("me@gmail.com"), "s-before": legacyRehome("old-pin"), "s-extra": legacyRehome("a@x.com")},
	})
	cfg.Claude.PrimaryName = "me@gmail.com"
	root := cfg.Claude.AccountsRoot
	led := state.Open(cfg.Claude)
	if err := os.WriteFile(cfg.Claude.ReservedFile(), []byte("me@gmail.com\t/work/a\na@x.com\t/work/b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := headless.Put(root, "me", headless.Token{Value: "sk-ant-oat01-" + strings.Repeat("a", 60), MintedMS: 1, Key: "dir:me"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	AdoptRefs(cfg, &out)
	if !strings.Contains(out.String(), "2 session re-homes") {
		t.Errorf("the adoption said: %q", out.String())
	}
	cfg.Claude.PrimaryName = "renamed" // any name now: the records name the dir
	for id, want := range map[string]string{"s-now": "renamed", "s-before": "renamed", "s-extra": "a@x.com"} {
		if got := ownerOf(t, cfg.Claude, id); got != want {
			t.Errorf("%s's owner = %q, want %q", id, got, want)
		}
	}
	held, err := accounts.ReadReservations(cfg.Claude)
	if err != nil || !held.Admits(accounts.PrimaryRef, "/work/a") || !held.Admits("a@x.com", "/work/b") || held.Held("me@gmail.com") {
		t.Errorf("reservations: %v (%v)", held, err)
	}
	if tokens, err := headless.Read(root); err != nil || tokens[accounts.PrimaryRef].Value == "" || len(tokens) != 1 {
		t.Errorf("tokens: %v (%v)", tokens, err)
	}
	if led.AdoptionPending() || led.LegacyNames() != nil {
		t.Error("the home is not marked adopted, or still carries the legacy section")
	}

	// Once: a record a later build wrote under a display name is that build's.
	data, _ := os.ReadFile(filepath.Join(root, "state.json"))
	before := string(data)
	out.Reset()
	AdoptRefs(cfg, &out)
	if data, _ := os.ReadFile(filepath.Join(root, "state.json")); string(data) != before || out.Len() != 0 {
		t.Errorf("a second run changed the home or spoke: %q", out.String())
	}
}

// A home that never pinned its primary recorded no name: its records are
// under the login's local part, the shape of a second home whose primary was
// never named. The local part is adopted, and a session --last answers.
func TestADerivedPrimaryNameIsAdopted(t *testing.T) {
	cfg := legacyHome(t, "yan@x.com", map[string]any{
		"sessions":   map[string]any{"s1": legacyRehome("yan")},
		"placements": map[string]any{"last": map[string]any{"dir:yan": map[string]any{"name": "yan", "at_ms": time.Now().UnixMilli()}}},
	})
	AdoptRefs(cfg, &bytes.Buffer{})
	cfg.Claude.PrimaryName = "pinned-later"
	if got := ownerOf(t, cfg.Claude, "s1"); got != "pinned-later" {
		t.Errorf("owner = %q", got)
	}
	if last, ok := state.Open(cfg.Claude).Load().Placements().Newest(cfg.Claude.AccountsRoot); !ok || last.Ref != accounts.PrimaryRef {
		t.Errorf("--last = %+v %v", last, ok)
	}
}

// A shell that pinned "primary" — the dotfiles' default until a host file
// names the primary — wrote its records there, and a build before any name
// was recorded left no section saying so. Those records are the primary's
// under whatever name it has now; before refs, renaming to the login's local
// part was taken for a stale shell and they stayed behind.
func TestRecordsUnderTheDefaultPinAreAdoptedUnderAnyName(t *testing.T) {
	cfg := legacyHome(t, "qiushi@x.com", map[string]any{
		"sessions": map[string]any{"s1": legacyRehome("primary")},
	})
	if err := os.WriteFile(cfg.Claude.ReservedFile(), []byte("primary\t/work/a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Claude.PrimaryName = "qiushi"
	AdoptRefs(cfg, &bytes.Buffer{})
	if got := ownerOf(t, cfg.Claude, "s1"); got != "qiushi" {
		t.Errorf("owner = %q", got)
	}
	if launchop.HeldAway(accounts.Discover(cfg.Claude), "qiushi", t.TempDir()) == false {
		t.Error("the reservation did not stay with the primary")
	}
}

// A `.reserved` that does not read stops the adoption before anything moves,
// says so, and leaves the home to the next run: a reservation is never
// rewritten from a guess, and a re-home moved without it would split the
// primary's records across two names.
func TestAnUnreadableReservationStopsTheAdoption(t *testing.T) {
	cfg := legacyHome(t, "me@x.com", map[string]any{"sessions": map[string]any{"s1": legacyRehome("me")}})
	if err := os.WriteFile(cfg.Claude.ReservedFile(), []byte("not a reservation line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	AdoptRefs(cfg, &out)
	if !strings.Contains(out.String(), "tries again") {
		t.Errorf("the refusal said: %q", out.String())
	}
	if !state.Open(cfg.Claude).AdoptionPending() {
		t.Error("the home was marked adopted with its records unmoved")
	}
	if o, _ := state.Open(cfg.Claude).Load().Owner("s1"); o.Account != "me" {
		t.Errorf("the re-home moved without its reservation: %+v", o)
	}
}
