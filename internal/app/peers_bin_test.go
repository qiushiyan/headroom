package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
)

// Two machines with the same accounts, each of whose figures age for the
// accounts it does not drive. This one's token for a@x.com has aged out, so it
// cannot ask; the other machine, reached through a stub ssh that runs the real
// binary under the other home, read a@x.com a minute ago. A refresh here pulls
// that reading, and limits — which pulls nothing — shows it, named for the
// machine it came from. What this machine hands back is its own alone.
func TestAReadingFromAnotherMachineIsShownHere(t *testing.T) {
	bin := headroomBinary(t)
	here := newBinHome(t, time.Now().Add(-time.Hour))
	there := newBinHome(t, time.Now().Add(time.Hour))

	// The other machine's own reading of a@x.com, and its headroom where
	// `make install` puts it.
	st := state.Open(there.scope)
	key := state.Key{UUID: "u-a@x.com", Name: "a@x.com"}
	at := time.Now().Add(-time.Minute)
	dec, err := st.Claim([]state.Key{key}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Complete(key, dec[0].Generation, state.OutcomeStored,
		[]byte(`{"limits":[{"kind":"weekly_all","group":"weekly","percent":88,"resets_at":"2099-01-01T00:00:00Z"}]}`), at); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(there.home, ".local", "bin")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(local, "headroom")); err != nil {
		t.Fatal(err)
	}
	// The other machine answers from disk alone: anything it ran there — the
	// Keychain, a vendor, ssh onward — leaves a mark.
	edges := filepath.Join(t.TempDir(), "edges")
	for _, name := range []string{"security", "claude", "ssh"} {
		if err := os.WriteFile(filepath.Join(there.stubs, name), fmt.Appendf(nil, "#!/bin/sh\necho %s >>%q\nexit 44\n", name, edges), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ssh runs the remote command, its last argument, as the other machine's
	// login shell would: under that home, with a PATH that lacks ~/.local/bin,
	// and that machine's own HEADROOM_PEERS naming this one back — so a
	// readings that pulled onward would reach the recording ssh.
	ssh := fmt.Sprintf("#!/bin/sh\nfor last; do :; done\nexec env -i HOME=%q PATH=%q HEADROOM_PEERS=mac /bin/sh -c \"$last\"\n", there.home, there.stubs+":/usr/bin:/bin")
	if err := os.WriteFile(filepath.Join(here.stubs, "ssh"), []byte(ssh), 0o755); err != nil {
		t.Fatal(err)
	}

	env := here.env("HEADROOM_PEERS=mini2", "HEADROOM_USAGE_URL=http://127.0.0.1:1")
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("headroom %v: %v\n%s", args, err, out)
		}
		return out
	}
	run("refresh", "--vendor", "claude")
	if b, err := os.ReadFile(edges); err == nil {
		t.Errorf("the other machine's readings ran %q; they are read from disk alone", b)
	}

	var doc jsonDoc
	if err := json.Unmarshal(run("limits", "--vendor", "claude", "--account", "a@x.com"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Accounts) != 1 || doc.Accounts[0].Usage == nil {
		t.Fatalf("limits = %+v, want a@x.com's figures", doc.Accounts)
	}
	u := doc.Accounts[0].Usage
	if u.Source != "peer" || u.Peer != "mini2" || len(u.Limits) != 1 || u.Limits[0].Percent != 88 {
		t.Errorf("a@x.com's usage = %+v, want mini2's 88%%", u)
	}

	var own struct{ Readings []struct{ Account string } }
	if err := json.Unmarshal(run("readings", "--vendor", "claude"), &own); err != nil {
		t.Fatal(err)
	}
	for _, r := range own.Readings {
		if r.Account == key.ID() {
			t.Errorf("this machine handed back the reading it pulled from mini2: %+v", own.Readings)
		}
	}

	// A vendor this machine lacks answers an empty list, never no list.
	if out := run("readings", "--vendor", "codex"); !strings.Contains(string(out), `"readings":[]`) {
		t.Errorf("readings for an absent vendor = %s, want an empty list", out)
	}

	// Without HEADROOM_PEERS the same home reads as a machine on its own.
	env = here.env("HEADROOM_USAGE_URL=http://127.0.0.1:1")
	if err := json.Unmarshal(run("limits", "--vendor", "claude", "--account", "a@x.com"), &doc); err != nil {
		t.Fatal(err)
	}
	if u := doc.Accounts[0].Usage; u != nil && u.Source == "peer" {
		t.Errorf("a home naming no peer showed a pulled reading: %+v", u)
	}
}

// pairedWith is f's scope naming mini2, holding mini2's reading of name —
// session and weekly percent, made now.
func (f autoFixture) pairedWith(name string, session, weekly int) config.Scope {
	f.t.Helper()
	paired := f.cfg
	paired.Peers = []string{"mini2"}
	st := state.Open(paired)
	now := time.Now()
	c, err := st.ClaimPull("mini2", now)
	if err != nil || !c.Permit {
		f.t.Fatalf("claim: %+v %v", c, err)
	}
	reset := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	body := fmt.Sprintf(`{"limits":[{"kind":"session","group":"session","percent":%d,"resets_at":%q},`+
		`{"kind":"weekly_all","group":"weekly","percent":%d,"resets_at":%q}]}`, session, reset(2*time.Hour), weekly, reset(72*time.Hour))
	if err := st.RecordPull("mini2", c.Generation, map[string]state.Observation{f.key(name).ID(): {FetchedAtMS: now.UnixMilli(), Body: []byte(body)}}, "", now); err != nil {
		f.t.Fatal(err)
	}
	return paired
}

// The board's next-launch mark and a headless placement choose on another
// machine's newer reading as a launch does: b's own figures make it the
// emptiest, mini2's newer reading of b says otherwise, and both move — while
// the same home naming no peer keeps choosing on its own figures.
func TestTheMarkAndAHeadlessPlacementFollowAnotherMachinesReading(t *testing.T) {
	f := newAutoFixture(t, "a@x.com", "b@x.com")
	f.observe("qiushi", 40, 10, time.Minute)
	f.observe("a@x.com", 20, 10, time.Minute)
	f.observe("b@x.com", 10, 10, time.Minute)
	f.mint("a@x.com", 300*24*time.Hour)
	f.mint("b@x.com", 300*24*time.Hour)
	paired := f.pairedWith("b@x.com", 60, 10)

	marked := func(scope config.Scope) string {
		r := round.FromDisk(scope, time.Now())
		markBoard(r, time.Now(), f.edges)
		for _, a := range r.Accounts() {
			if a.View.Next {
				return a.Acct.Name
			}
		}
		return ""
	}
	if got := marked(f.cfg); got != "b@x.com" {
		t.Errorf("on its own figures the board marks %q, want b", got)
	}
	if got := marked(paired); got != "a@x.com" {
		t.Errorf("with mini2's newer reading of b the board marks %q, want a", got)
	}

	doc, code, errw := f.place("--dry-run")
	if code != 0 || doc.Account != "b@x.com" {
		t.Errorf("on its own figures a headless placement takes %q (exit %d, %s), want b", doc.Account, code, errw)
	}
	f.cfg = paired
	doc, code, errw = f.place("--dry-run")
	if code != 0 || doc.Account != "a@x.com" {
		t.Errorf("with mini2's newer reading of b a headless placement takes %q (exit %d, %s), want a", doc.Account, code, errw)
	}
}
