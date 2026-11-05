package config

// `.ledger`: which accounts root's ledger a Claude Code home spends against.
// Its one owner: Load reads it strictly, ShareLedger writes it by the same
// rule, and LedgerOf is how one home asks about another.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/atomicfile"
)

// LedgerFile names the accounts root whose state.json this home's subscription
// ledger lives in (one absolute path on one line). Absent means this home's
// own. Configuration a human or `headroom accounts ledger` writes; read
// strictly by Load.
func (s Scope) LedgerFile() string { return filepath.Join(s.AccountsRoot, ".ledger") }

// Ledger is the accounts root whose state.json holds the subscription ledger.
func (s Scope) Ledger() string {
	if s.LedgerRoot == "" {
		return s.AccountsRoot
	}
	return s.LedgerRoot
}

// readLedger reads the Claude Code scope's `.ledger`, strictly. Absent is
// this home's own ledger. Anything present must pass ledgerRoot; a file that
// does not refuses, the way a relative override does: a home that meant to
// share a ledger and quietly kept its own would ask every subscription twice
// per spacing and pile launches onto accounts the other home is filling —
// silently.
func readLedger(claude, codex Scope) (string, error) {
	path := claude.LedgerFile()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("%s could not be read (%v) — it names the accounts root whose ledger this home shares", path, err)
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("%s is empty — it must name the accounts root whose ledger this home shares, or be deleted", path)
	}
	root, err := ledgerRoot(claude, codex, v)
	if err != nil {
		return "", fmt.Errorf("%s: %v — correct it, or delete it to keep this home's own ledger", path, err)
	}
	return root, nil
}

// ShareLedger records which accounts root's ledger this home's Claude Code
// accounts spend against, validated by the rule Load reads the file with, so
// the two cannot disagree. Naming this home's own root removes the file — the
// home keeps its own ledger again. It returns the root recorded, "" for this
// home's own.
func (c Config) ShareLedger(v string) (string, error) {
	root, err := ledgerRoot(c.Claude, c.Codex, v)
	switch {
	case err != nil:
		return "", err
	case root == "":
		if err := os.Remove(c.Claude.LedgerFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return "", nil
	}
	if err := os.MkdirAll(c.Claude.AccountsRoot, 0o755); err != nil {
		return "", err
	}
	return root, atomicfile.Write(c.Claude.LedgerFile(), ".ledger-*", []byte(root+"\n"))
}

// ledgerRoot validates v as the accounts root whose ledger a Claude Code home
// shares: one absolute path to an existing directory, other than the Codex
// accounts root. It returns the cleaned path, or "" when v names this home's
// own root — sharing one's own ledger is having one's own.
func ledgerRoot(claude, codex Scope, v string) (string, error) {
	switch {
	case strings.ContainsAny(v, "\n\r"):
		return "", errors.New("a ledger is one accounts root, on one line")
	case !filepath.IsAbs(v):
		return "", fmt.Errorf("%q is not absolute", v)
	case !isDir(v):
		return "", fmt.Errorf("%s is not a directory — it must be the accounts root whose state.json the homes share", v)
	case sameLocation(v, codex.AccountsRoot):
		return "", fmt.Errorf("%s is the Codex accounts root — a Claude Code ledger cannot live there", v)
	case sameLocation(v, claude.AccountsRoot):
		return "", nil
	}
	// A ledger is a place, not a pointer to follow: a root that spends
	// against another root's ledger holds no ledger of its own for this home
	// to share, and two roots naming each other would each spend against the
	// other's file while both read as shared.
	if target := LedgerOf(v); !SameDir(target, v) {
		return "", fmt.Errorf("%s itself spends against %s's ledger — name that root instead", v, target)
	}
	return filepath.Clean(v), nil
}

// LedgerOf is the accounts root whose ledger the home at accountsRoot spends
// against today: what its `.ledger` names, or the root itself when it has
// none. It reads the file and judges nothing — Load is where a home's own
// file is held to the rules; this is how one home asks about another.
func LedgerOf(accountsRoot string) string {
	data, err := os.ReadFile(filepath.Join(accountsRoot, ".ledger"))
	if v := strings.TrimSpace(string(data)); err == nil && v != "" {
		return filepath.Clean(v)
	}
	return filepath.Clean(accountsRoot)
}
