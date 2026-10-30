package accounts

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/qiushiyan/headroom/internal/config"
)

// RenamePrimaryFiles moves what `.current` and `.reserved` hold under the
// primary's old name to its new one, and says how many records moved. A
// `.current` naming another account, or "auto", is left alone; the old name's
// reservations join any the new name already has. A `.reserved` that does not
// read is an error and nothing is written: a reservation is a person's
// statement, never rewritten from a guess. The caller holds the store's lock
// (state.Store.RenamePrimary).
func RenamePrimaryFiles(cfg config.Scope, from, to string) (int, error) {
	// Both read before either is written, so a refusal moves nothing.
	held, err := ReadReservations(cfg)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(cfg.CurrentFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	moved := 0
	if err == nil && strings.TrimSpace(string(data)) == from {
		if err := writeLine(cfg.AccountsRoot, cfg.CurrentFile(), to); err != nil {
			return moved, err
		}
		moved++
	}
	if dirs, ok := held[from]; ok {
		for _, d := range dirs {
			if !slices.Contains(held[to], d) {
				held[to] = append(held[to], d)
			}
		}
		delete(held, from)
		if err := writeReservations(cfg, held); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}
