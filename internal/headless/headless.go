// Package headless keeps the credentials a headless caller spends the
// subscriptions with: one long-lived token per account, minted by the
// vendor's `claude setup-token`, stored on this machine in a file only the
// user can read.
//
// A token is a separate grant. It never refreshes, and nothing about it
// touches the account's interactive login, which is why a caller that brings
// its own config dir is handed a token and never the login itself: a login
// lent to another config dir would be refreshed from a dir that does not hold
// its lock.
package headless

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/qiushiyan/headroom/internal/atomicfile"
)

// fileName is the token file, beside the accounts it holds tokens for.
const fileName = "headless.json"

// Path is the token file of one Claude Code accounts root.
func Path(accountsRoot string) string { return filepath.Join(accountsRoot, fileName) }

// Token is one account's headless credential and what was known when it was
// minted. Key is the subscription the account's dir was logged in to then —
// the ledger key its load is counted under — and Email whom.
type Token struct {
	Value    string `json:"token"`
	MintedMS int64  `json:"minted_at_ms"`
	EndsMS   int64  `json:"ends_at_ms,omitempty"` // 0 = the vendor did not say
	Key      string `json:"account"`
	Email    string `json:"email,omitempty"`
}

// Ended reports positive evidence that the token no longer works: its end is
// known and has passed. An unknown end is never read as ended.
func (t Token) Ended(now time.Time) bool { return t.EndsMS > 0 && now.UnixMilli() >= t.EndsMS }

// tokenShape is the vendor's OAuth token: Claude Code reads any credential
// that starts "sk-ant-oat" as one (2.1.296), and a setup-token is printed as
// sk-ant-oat01-… over the URL-safe base64 alphabet.
var tokenShape = regexp.MustCompile(`^sk-ant-oat[0-9]{2}-[A-Za-z0-9_-]{20,}$`)

// valid reports whether s has the shape of a vendor OAuth token.
func valid(s string) bool { return tokenShape.MatchString(s) }

// file is the document on disk.
type file struct {
	V      int              `json:"v"`
	Tokens map[string]Token `json:"tokens"`
}

const version = 1

// ErrExposed is Read's answer for a file other users can read: a secret that
// has been exposed is not handed out until the user has seen that it was.
var ErrExposed = errors.New("readable by other users")

// Read returns every stored token by account name. An absent file holds none.
// A file that cannot be read or decoded, or that others can read, is an
// error: it is never taken for an empty one, which a write would then replace.
func Read(accountsRoot string) (map[string]Token, error) {
	path := Path(accountsRoot)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Token{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is %w (mode %v) — `chmod 600 %s`, and mint again any token that may have been read", path, ErrExposed, info.Mode().Perm(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s is not readable (%v) — move it aside and mint again", path, err)
	}
	if f.V > version {
		return nil, fmt.Errorf("%s was written by a newer headroom", path)
	}
	if f.Tokens == nil {
		f.Tokens = map[string]Token{}
	}
	return f.Tokens, nil
}

// Put stores name's token, replacing any it had.
func Put(accountsRoot, name string, t Token) error {
	if !valid(t.Value) {
		return errors.New("not a vendor OAuth token")
	}
	return update(accountsRoot, func(tokens map[string]Token) bool {
		tokens[name] = t
		return true
	})
}

// Remove drops name's token and reports whether there was one.
func Remove(accountsRoot, name string) (bool, error) {
	removed := false
	err := update(accountsRoot, func(tokens map[string]Token) bool {
		_, removed = tokens[name]
		delete(tokens, name)
		return removed
	})
	return removed, err
}

// update is the one writer: a locked read, change and atomic replace. The
// file is created readable by the user alone and stays so; a file that does
// not read is refused, never written over.
func update(accountsRoot string, change func(map[string]Token) bool) error {
	if err := os.MkdirAll(accountsRoot, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(Path(accountsRoot)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	tokens, err := Read(accountsRoot)
	if err != nil {
		return err
	}
	if !change(tokens) {
		return nil
	}
	data, err := json.MarshalIndent(file{V: version, Tokens: tokens}, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(Path(accountsRoot), ".headless-*.json", data) // 0600
}
