package app

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// keptFrom says a bare launch from dir leaves the account named name out
// because a reservation keeps it for other work.
func keptFrom(t *testing.T, scope config.Scope, name, dir string) bool {
	t.Helper()
	t.Chdir(dir)
	for line := range strings.SplitSeq(launchOn(t, scope, quietPlace(), "--dry-run").stdout, "\n") {
		if strings.Contains(line, name) && strings.Contains(line, "excluded: reserved for") {
			return true
		}
	}
	return false
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
	if note := out.String(); !strings.Contains(note, "2 session re-homes") || !strings.Contains(note, "old-pin") || strings.Contains(note, "primary,") || strings.HasSuffix(strings.TrimSpace(note), "primary") {
		t.Errorf("the adoption said: %q — it names what moved, and only the names it moved from", note)
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
	if done, _ := led.Adoption(); !done || led.LegacyNames() != nil {
		t.Error("the home is not marked adopted, or still carries the legacy section")
	}

	// Once: a record a later build wrote under a display name is that build's.
	before := homeTree(t, cfg.Claude.Home)
	out.Reset()
	AdoptRefs(cfg, &out)
	if after := homeTree(t, cfg.Claude.Home); !maps.Equal(before, after) || out.Len() != 0 {
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
	if out := launchOn(t, cfg.Claude, quietPlace(), "--last", "--dry-run").stdout; !strings.Contains(out, "would start claude on pinned-later") {
		t.Errorf("--last after adopting:\n%s", out)
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
	if !keptFrom(t, cfg.Claude, "qiushi", t.TempDir()) {
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
	if done, _ := state.Open(cfg.Claude).Adoption(); done {
		t.Error("the home was marked adopted with its records unmoved")
	}
	if o, _ := state.Open(cfg.Claude).Load().Owner("s1"); o.Account != "me" {
		t.Errorf("the re-home moved without its reservation: %+v", o)
	}
}

// An adopted home is done, whatever sits beside its store: no later command
// takes the store's lock for the adoption or changes the home, so one that
// another process holds never stalls a command. A home whose only legacy
// file is a `.reserved` is adopted once, like any other, and then left alone.
func TestAnAdoptedHomeIsLeftAlone(t *testing.T) {
	for name, doc := range map[string]map[string]any{
		"adopted":         {"refs": 1},
		"no document yet": nil,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := legacyHome(t, "me@x.com", map[string]any{})
			root := cfg.Claude.AccountsRoot
			if doc == nil {
				os.Remove(filepath.Join(root, "state.json"))
			} else {
				b, _ := json.Marshal(map[string]any{"version": 1, "refs": 1})
				os.WriteFile(filepath.Join(root, "state.json"), b, 0o600)
			}
			if err := os.WriteFile(cfg.Claude.ReservedFile(), []byte("a@x.com\t/work/b\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if doc == nil {
				AdoptRefs(cfg, &bytes.Buffer{}) // the one adoption: it marks the home
			}
			lock, err := os.OpenFile(filepath.Join(root, "state.json.lock"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			before := homeTree(t, cfg.Claude.Home)
			var out bytes.Buffer
			start := time.Now()
			AdoptRefs(cfg, &out)
			if took := time.Since(start); took > 500*time.Millisecond {
				t.Errorf("an adopted home waited %s for the store's lock", took)
			}
			if after := homeTree(t, cfg.Claude.Home); !maps.Equal(before, after) || out.Len() != 0 {
				t.Errorf("an adopted home was changed or spoke: %q\nbefore %v\nafter  %v", out.String(), before, after)
			}
		})
	}
}

// homeTree is every path under root with a file's bytes, a link's target or
// "dir": what a command must leave as it was.
func homeTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "-> " + target
		case d.IsDir():
			out[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			out[rel] = string(b)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Every command adopts before it runs: through the built binary, the first
// command a legacy home sees — here the shell preflight's resolve — moves its
// records and says so, and its own answer is unchanged. Losing the adoption
// from the command line's configuration load must fail this.
func TestTheFirstCommandAdoptsTheHome(t *testing.T) {
	bin := headroomBinary(t)
	cfg := legacyHome(t, "me@gmail.com", map[string]any{
		"primary":  map[string]any{"name": "me@gmail.com"},
		"sessions": map[string]any{"s1": legacyRehome("me@gmail.com")},
	})
	if err := os.WriteFile(cfg.Claude.ReservedFile(), []byte("me@gmail.com\t/work/a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "resolve", "me@gmail.com")
	cmd.Env = append(os.Environ(), "HEADROOM_HOME="+cfg.Claude.Home, "HEADROOM_PRIMARY_NAME=me@gmail.com")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("resolve: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "me@gmail.com\t") || !strings.HasSuffix(strings.TrimSpace(stdout.String()), "primary") {
		t.Errorf("resolve's answer: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "now name its dir") {
		t.Errorf("the adoption said nothing: %q", stderr.String())
	}
	if o, _ := state.Open(cfg.Claude).Load().Owner("s1"); o.Account != accounts.PrimaryRef {
		t.Errorf("the re-home after the command: %+v", o)
	}
	if held, _ := accounts.ReadReservations(cfg.Claude); !held.Admits(accounts.PrimaryRef, "/work/a") {
		t.Errorf("reservations after the command: %v", held)
	}
}

// A home that spends against another's ledger — steward's shape: its primary
// unpinned, its re-homes under its login's local part in its own document,
// its --last in the owner's ledger — adopts its records in both files, marks
// only its own document, and leaves the owner's entry, which is the owner's
// to name, as it was.
func TestASecondHomeAdoptsItsRecordsInBothFiles(t *testing.T) {
	f := newTwoHomes(t)
	write := func(root string, doc map[string]any) {
		doc["version"] = 1
		b, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	write(f.steward.AccountsRoot, map[string]any{"sessions": map[string]any{"s1": legacyRehome("yan")}})
	write(f.owner.AccountsRoot, map[string]any{"placements": map[string]any{"by_home": map[string]any{
		f.steward.AccountsRoot: map[string]any{"name": "yan", "at_ms": now},
		f.owner.AccountsRoot:   map[string]any{"name": "qiushi", "at_ms": now},
	}}})

	AdoptRefs(config.Config{Claude: f.steward, Codex: config.ForHome(f.steward.Home).Codex}, &bytes.Buffer{})
	if o, _ := state.Open(f.steward).Load().Owner("s1"); o.Account != accounts.PrimaryRef {
		t.Errorf("the second home's re-home: %+v", o)
	}
	if out := launchOn(t, f.steward, f.edges, "--last", "--dry-run").stdout; !strings.Contains(out, "would start claude on yan") {
		t.Errorf("the second home's --last after adopting:\n%s", out)
	}
	if last, _ := state.Open(f.owner).Load().Placements().Newest(f.owner.AccountsRoot); last.Ref != "qiushi" {
		t.Errorf("the owner's --last was moved by the second home: %+v", last)
	}
	if done, _ := state.Open(f.steward).Adoption(); !done {
		t.Error("the second home is not marked adopted")
	}
	if done, _ := state.Open(f.owner).Adoption(); done {
		t.Error("the owner was marked adopted by the second home's adoption")
	}
}

// Two commands that both find the home unadopted move it once: the second
// waits for the first's lock, finds the marker and moves nothing, so one of
// them says what moved and the other says nothing.
func TestCommandsAdoptingTogetherMoveTheHomeOnce(t *testing.T) {
	cfg := legacyHome(t, "me@x.com", map[string]any{
		"primary":  map[string]any{"name": "me"},
		"sessions": map[string]any{"s1": legacyRehome("me"), "s2": legacyRehome("me")},
	})
	var outs [2]bytes.Buffer
	var wg sync.WaitGroup
	for i := range outs {
		wg.Go(func() { AdoptRefs(cfg, &outs[i]) })
	}
	wg.Wait()
	said := 0
	for i := range outs {
		if strings.Contains(outs[i].String(), "2 session re-homes") {
			said++
		} else if outs[i].Len() != 0 {
			t.Errorf("a command said: %q", outs[i].String())
		}
	}
	if said != 1 {
		t.Errorf("%d commands said they moved the records, want one: %q / %q", said, outs[0].String(), outs[1].String())
	}
	for _, id := range []string{"s1", "s2"} {
		if o, _ := state.Open(cfg.Claude).Load().Owner(id); o.Account != accounts.PrimaryRef {
			t.Errorf("%s: %+v", id, o)
		}
	}
}
