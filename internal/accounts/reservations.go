package accounts

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qiushiyan/headroom/internal/atomicfile"
	"github.com/qiushiyan/headroom/internal/config"
)

// Reservations is `.reserved`: the directories each account is kept for, by
// account name. A reservation is a person's statement of what an account is
// for, so it lives beside `.current` and not in the store, whose bookkeeping
// sets aside what it cannot decode: losing this file would quietly release
// every account in it.
type Reservations map[string][]string

// ReadReservations reads `.reserved`, strictly, and is its one reader. An
// absent file means no reservations. Anything else it cannot read, an empty
// file included, is an error: `accounts reserve` removes the file with its
// last row, so an empty one is damage, and damage says nothing about which
// accounts are kept for what.
func ReadReservations(cfg config.Scope) (Reservations, error) {
	data, err := os.ReadFile(cfg.ReservedFile())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf(".reserved could not be read (%v)", err)
	case len(data) == 0:
		return nil, errors.New(".reserved is empty")
	}
	held := Reservations{}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		name, dir, ok := strings.Cut(line, "\t")
		if !ok || name == "" || !filepath.IsAbs(dir) {
			return nil, fmt.Errorf(".reserved line %d is not <account><TAB><absolute dir>", i+1)
		}
		held[name] = append(held[name], dir)
	}
	return held, nil
}

// Held says the account has a reservation. It stays kept from everything else
// whether or not its directories still exist, so losing a repository never
// releases it.
func (r Reservations) Held(name string) bool { return len(r[name]) > 0 }

// Admits says project is at or under one of the account's directories,
// compared by path component so ~/dev/itell-cms is not inside ~/dev/itell. An
// unknown project ("") is inside nothing.
func (r Reservations) Admits(name, project string) bool {
	if project == "" {
		return false
	}
	return slices.ContainsFunc(r[name], func(dir string) bool {
		return project == dir || strings.HasPrefix(project, strings.TrimSuffix(dir, "/")+"/")
	})
}

// SetReservation records the directories an account is kept for, replacing
// any it had; no directories drops its reservation. The file is replaced
// atomically and removed with its last row.
func SetReservation(cfg config.Scope, name string, dirs []string) error {
	held, err := ReadReservations(cfg)
	if err != nil {
		return err
	}
	if len(dirs) == 0 && !held.Held(name) {
		return nil
	}
	if held == nil {
		held = Reservations{}
	}
	if len(dirs) == 0 {
		delete(held, name)
	} else {
		held[name] = dirs
	}
	return writeReservations(cfg, held)
}

// writeReservations replaces `.reserved` with held, removing it with its
// last row.
func writeReservations(cfg config.Scope, held Reservations) error {
	if len(held) == 0 {
		return os.Remove(cfg.ReservedFile())
	}
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(held)) {
		for _, d := range held[n] {
			fmt.Fprintf(&b, "%s\t%s\n", n, d)
		}
	}
	return atomicfile.Write(cfg.ReservedFile(), ".reserved-*", []byte(b.String()))
}
