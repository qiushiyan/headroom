package sessions

import (
	"os"
	"path/filepath"
	"strings"
)

// RepoIdentity groups sessions by repository without spawning git: a
// worktree's .git is a *file* containing "gitdir: <main>/.git/worktrees/<n>",
// a main checkout's .git is a directory — so identity is one small read.
// Sixty distinct dirs cost microseconds; sixty `git rev-parse` spawns cost
// half a second and drag exec into a package whose whole job is reading
// files.
type repoInfo struct {
	Key  string // canonical main-checkout path; "" = not in a repo
	Root string // the dir that held .git — the checkout/worktree root
}

// repoCache memoizes per starting dir; the walker hits the same few dirs
// hundreds of times.
type repoCache map[string]repoInfo

// lookup walks up from dir to the nearest .git. A missing dir has no
// identity — a deleted worktree can never be classified local, only global.
func (c repoCache) lookup(dir string) repoInfo {
	if dir == "" {
		return repoInfo{}
	}
	if info, ok := c[dir]; ok {
		return info
	}
	info := c.resolve(dir)
	c[dir] = info
	return info
}

func (c repoCache) resolve(dir string) repoInfo {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return repoInfo{}
	}
	for d := dir; ; {
		gitPath := filepath.Join(d, ".git")
		if fi, err := os.Lstat(gitPath); err == nil {
			if fi.IsDir() {
				return repoInfo{Key: canon(d), Root: d}
			}
			if fi.Mode().IsRegular() {
				if main, ok := worktreeMain(gitPath); ok {
					return repoInfo{Key: canon(main), Root: d}
				}
				return repoInfo{Key: canon(d), Root: d} // unreadable gitdir: itself
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return repoInfo{}
		}
		d = parent
	}
}

// worktreeMain extracts the main checkout from a worktree's .git file:
// "gitdir: /path/to/main/.git/worktrees/<name>".
func worktreeMain(gitFile string) (string, bool) {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(data))
	gitdir, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", false
	}
	gitdir = strings.TrimSpace(gitdir)
	if main, _, found := strings.Cut(gitdir, "/.git/worktrees/"); found {
		return main, true
	}
	// A .git file with an unexpected shape (submodule, future layout):
	// no honest main checkout to name.
	return "", false
}

// canon resolves symlinks so two spellings of one checkout compare equal.
func canon(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}
