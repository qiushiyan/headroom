package app

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
)

// ReserveOptions are what `accounts reserve` asks: the account, the
// directories it is for, or that it is for none any more.
type ReserveOptions struct {
	Account string
	Dirs    []string
	Clear   bool
}

// AccountsReserve lists what is kept for what, or changes one account's
// reservation. A directory is resolved as a launch's will be, so a worktree
// names its repository and the row a launch compares against is the one
// written here.
func AccountsReserve(std Streams, cfg config.Scope, opts ReserveOptions) error {
	out := std.Out
	switch {
	case opts.Account == "" && (opts.Clear || len(opts.Dirs) > 0):
		return usagef("name the account")
	case opts.Clear && len(opts.Dirs) > 0:
		return usagef("--clear drops the reservation, so it takes no directories")
	case opts.Account == "":
		held, err := accounts.ReadReservations(cfg)
		if err != nil {
			return err
		}
		if len(held) == 0 {
			fmt.Fprintln(out, "no account is reserved")
		}
		for _, name := range slices.Sorted(maps.Keys(held)) {
			fmt.Fprintf(out, "%s  %s\n", render.Sanitize(name), reservedDirs(held[name]))
		}
		return nil
	case opts.Clear:
		// By name alone: a row outlives its account until someone clears it.
		if err := accounts.SetReservation(cfg, opts.Account, nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s is no longer reserved\n", render.Sanitize(opts.Account))
		return nil
	case len(opts.Dirs) == 0:
		return usagef("name the repositories %s is for, or --clear", opts.Account)
	}
	a, err := accounts.Discover(cfg).Select(opts.Account)
	if err != nil {
		return err
	}
	var dirs []string
	for _, d := range opts.Dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return err
		}
		key := sessions.RepoKey(abs)
		switch {
		case key == "":
			return fmt.Errorf("%s is not a directory", d)
		case strings.ContainsAny(key, "\t\n"):
			return fmt.Errorf("%q cannot be written to .reserved, which holds one row per line", key)
		case !slices.Contains(dirs, key):
			dirs = append(dirs, key)
		}
	}
	if err := accounts.SetReservation(cfg, a.Name, dirs); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s reserved for %s\n", render.Sanitize(a.Name), reservedDirs(dirs))
	return nil
}

func reservedDirs(dirs []string) string {
	short := make([]string, len(dirs))
	for i, d := range dirs {
		short[i] = render.Sanitize(render.Tilde(d))
	}
	return strings.Join(short, ", ")
}
