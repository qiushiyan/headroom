package sessions

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RepoKey is what a directory belongs to: the main checkout of the repository
// it is in, whichever worktree it reached it through, or its own canonical path
// when it is in none. "" when it is not an absolute path to a directory, so a
// caller can tell nothing known from a match. The path is canonicalized before
// the walk: a symlink into a worktree's subdirectory has no .git above its
// spelling, only above what it resolves to.
//
// Identity is read without spawning git: a worktree's .git is a *file*
// containing "gitdir: <main>/.git/worktrees/<n>", a main checkout's .git is a
// directory — so it is one small read per level.
func RepoKey(dir string) string {
	if !filepath.IsAbs(dir) {
		return ""
	}
	dir = canon(dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	// A .git that names no git directory is a husk, not a checkout — the live
	// store has one holding nothing but info/exclude, several directories
	// below the repository it belongs to. Stopping there would key work on a
	// path git does not consider a checkout, so the walk keeps climbing and
	// only settles for the husk when nothing above it is real.
	husk := ""
	for d := dir; ; {
		key, found, real := checkoutAt(d)
		if real {
			return key
		}
		if found && husk == "" {
			husk = key
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if husk != "" {
		return husk
	}
	return dir
}

// checkoutAt reads the .git at dir, if there is one: the key a checkout
// there belongs to (its main checkout's, for a worktree), whether there is a
// .git at all, and whether it is a real checkout — its git directory holds a
// HEAD, the evidence that tells a husk from the genuine article.
func checkoutAt(dir string) (key string, found, real bool) {
	gitPath := filepath.Join(dir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		return "", false, false
	}
	key, gitdir := canon(dir), gitPath
	switch {
	case fi.IsDir():
	case fi.Mode().IsRegular():
		var ok bool
		if gitdir, ok = gitFileDir(gitPath, dir); !ok {
			// A .git file that cannot be followed may still be the checkout,
			// but nothing here can name its main repository.
			return key, true, false
		}
		if main, _, ok := strings.Cut(gitdir, "/.git/worktrees/"); ok {
			key = canon(main)
		}
		// Any other shape (submodule, future layout) has no honest main
		// checkout to name, so the key stays this dir.
	default:
		return "", false, false
	}
	_, err = os.Stat(filepath.Join(gitdir, "HEAD"))
	return key, true, !errors.Is(err, fs.ErrNotExist)
}

// gitFileDir resolves the git directory a .git *file* points at:
// "gitdir: /path/to/main/.git/worktrees/<name>". Submodules commonly spell
// that path relative to the checkout, so it is resolved against it.
func gitFileDir(gitFile, checkout string) (string, bool) {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", false
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	gitdir = strings.TrimSpace(gitdir)
	if gitdir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(checkout, gitdir)
	}
	return filepath.Clean(gitdir), true
}

// canon resolves symlinks so two spellings of one checkout compare equal.
func canon(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}
