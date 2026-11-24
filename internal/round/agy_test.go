package round

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/state"
)

// agyScope is a present Antigravity under a fixture home, with an agy on PATH
// that answers for email.
func agyScope(t *testing.T, email string) config.Scope {
	t.Helper()
	home := t.TempDir()
	scope := config.ForHome(home).Agy
	scope.Present = true
	bin := filepath.Join(home, "bin")
	for _, d := range []string{scope.PrimaryDir(), bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
log=""
while [ $# -gt 0 ]; do [ "$1" = "--log-file" ] && log="$2"; shift; done
echo 'I1124 1 server_oauth.go:221] applyAuthResult: email=` + email + `, authMethod=consumer' > "$log"
echo '{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":0.6,"reset_time":"2099-01-01T00:00:00Z"}]}]}}}'
`
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return scope
}

// Antigravity's one account is the machine's sign-in: usable as far as
// headroom can tell, asked by running agy, and named by the email its
// reading was measured for — live, and from disk afterwards.
func TestAgyRoundReadsTheOneSignIn(t *testing.T) {
	scope := agyScope(t, "me@gmail.com")
	st := state.Open(scope)
	r := Prepare(context.Background(), scope, st, time.Now(), Deps{})
	accts := r.Accounts()
	if len(accts) != 1 || accts[0].Acct.Name != "primary" || !accts[0].Acct.IsPrimary() {
		t.Fatalf("accounts %+v", accts)
	}
	if v := accts[0].View; v.Health != accountstate.HealthOK || v.Attempt.State != accountstate.AttemptPending || v.Label != "primary" || v.Launcher != "agy" {
		t.Errorf("before the read: %+v", v)
	}
	if !r.Asking() {
		t.Fatal("no request prepared")
	}
	r.Complete(context.Background())
	v := r.Accounts()[0].View
	if v.Attempt.State != accountstate.AttemptOK || v.Obs == nil || v.Obs.Rows[0].Percent != 40 || v.Label != "me@gmail.com" || v.Health != accountstate.HealthOK {
		t.Fatalf("after the read: %+v", v)
	}

	disk := FromDisk(scope, time.Now()).Accounts()[0].View
	if disk.Label != "me@gmail.com" || disk.Obs == nil || disk.Obs.Source != accountstate.SourceStore || disk.Health != accountstate.HealthUnprobed {
		t.Errorf("from disk: %+v", disk)
	}
}

// Whatever lands in the accounts root beside headroom's state file is no
// account: there is one sign-in per macOS user.
func TestAgyDiscoversThePrimaryAlone(t *testing.T) {
	scope := agyScope(t, "me@gmail.com")
	if err := os.MkdirAll(filepath.Join(scope.AccountsRoot, "b@x.com"), 0o755); err != nil {
		t.Fatal(err)
	}
	if accts := FromDisk(scope, time.Now()).Accounts(); len(accts) != 1 || !accts[0].Acct.IsPrimary() {
		t.Errorf("accounts %+v", accts)
	}
}
