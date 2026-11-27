package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	// login shell would: under that home, with a PATH that lacks ~/.local/bin.
	ssh := fmt.Sprintf("#!/bin/sh\nfor last; do :; done\nexec env -i HOME=%q PATH=%q /bin/sh -c \"$last\"\n", there.home, there.stubs+":/usr/bin:/bin")
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

	// Without HEADROOM_PEERS the same home reads as a machine on its own.
	env = here.env("HEADROOM_USAGE_URL=http://127.0.0.1:1")
	if err := json.Unmarshal(run("limits", "--vendor", "claude", "--account", "a@x.com"), &doc); err != nil {
		t.Fatal(err)
	}
	if u := doc.Accounts[0].Usage; u != nil && u.Source == "peer" {
		t.Errorf("a home naming no peer showed a pulled reading: %+v", u)
	}
}
