package app

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
)

// Antigravity keeps one sign-in per macOS user: no command adds, removes or
// reserves an account for it, and none of them touches the disk trying.
func TestAccountChangesRefuseAntigravity(t *testing.T) {
	scope := config.ForHome(t.TempDir()).Agy
	scope.Present = true
	for name, run := range map[string]func(std Streams) error{
		"add": func(std Streams) error { return AccountsAdd(std, scope, AddOptions{Email: "a@x.com"}) },
		"remove": func(std Streams) error {
			return AccountsRemove(std, Terminal{}, scope, "a@x.com", true, nil)
		},
		"reserve": func(std Streams) error {
			return AccountsReserve(std, scope, ReserveOptions{Account: "primary", Dirs: []string{t.TempDir()}})
		},
	} {
		var out bytes.Buffer
		err := run(Streams{In: bytes.NewReader(nil), Out: &out, Err: &out})
		if !errors.Is(err, accounts.ErrOneSignIn) || out.Len() != 0 {
			t.Errorf("%s: %v, printed %q; want the one-sign-in refusal and nothing printed", name, err, out.String())
		}
	}
	if _, err := os.Stat(scope.AccountsRoot); !os.IsNotExist(err) {
		t.Errorf("an account change touched the accounts root: %v", err)
	}
}
