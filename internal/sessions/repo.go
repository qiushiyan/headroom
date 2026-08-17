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
//
// The same walk yields the checkout's *live* HEAD, because branch is state,
// not identity: a transcript's gitBranch is an honest observation of the
// moment it was written, and the directory keeps moving after the session
// goes idle. Reading HEAD here is what keeps a row's label a statement about
// now rather than a fossil — see Head.
type repoInfo struct {
	Key  string // canonical main-checkout path; "" = not in a repo
	Root string // the dir that held .git — the checkout/worktree root
	Head Head   // what Root is checked out on right now
}

// HeadKind classifies what a checkout points at. Detached is a *state*, not
// a branch name: the vendor writes the literal string "HEAD" into transcripts
// when it samples one, and passing that through as a label names nothing.
type HeadKind uint8

const (
	HeadUnknown  HeadKind = iota // not a repo, or HEAD absent/unreadable/unparseable
	HeadBranch                   // on a named branch
	HeadDetached                 // at a bare commit — sha checkout, bisect
	HeadRebasing                 // detached with a rebase in flight
)

// Head is a checkout's HEAD as of the collect that read it. Branch carries
// the branch being replayed when Kind is HeadRebasing, and may be empty there
// (a rebase started from a detached HEAD has no branch to name).
type Head struct {
	Kind   HeadKind
	Branch string
	Commit string // abbreviated sha, when detached
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
				return repoInfo{Key: canon(d), Root: d, Head: readHead(gitPath)}
			}
			if fi.Mode().IsRegular() {
				gitdir, ok := gitFileDir(gitPath, d)
				if !ok {
					// A .git file we cannot follow: the checkout is still
					// itself, but nothing here can name its branch.
					return repoInfo{Key: canon(d), Root: d}
				}
				info := repoInfo{Key: canon(d), Root: d, Head: readHead(gitdir)}
				if main, _, found := strings.Cut(gitdir, "/.git/worktrees/"); found {
					info.Key = canon(main)
				}
				// Any other shape (submodule, future layout) has no honest
				// main checkout to name, so Key stays this dir — but its
				// gitdir still holds a readable HEAD.
				return info
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return repoInfo{}
		}
		d = parent
	}
}

// gitFileDir resolves the git directory a .git *file* points at:
// "gitdir: /path/to/main/.git/worktrees/<name>". Submodules commonly spell
// that path relative to the checkout, so it is resolved against it.
func gitFileDir(gitFile, checkout string) (string, bool) {
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
	if gitdir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(checkout, gitdir)
	}
	return filepath.Clean(gitdir), true
}

// readHead reads one file to answer "what is checked out here right now".
// Every failure degrades to HeadUnknown, which the label layer answers by
// falling back to path identity or to the transcript's observation — a
// checkout whose HEAD cannot be read must lose the annotation, never the row.
func readHead(gitdir string) Head {
	data, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return Head{}
	}
	line := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(line, "ref:"); ok {
		branch, ok := strings.CutPrefix(strings.TrimSpace(ref), "refs/heads/")
		if !ok || branch == "" {
			// A symref outside refs/heads is not a branch this can name.
			return Head{}
		}
		return Head{Kind: HeadBranch, Branch: branch}
	}
	if !isHex(line) || len(line) < 7 {
		return Head{}
	}
	h := Head{Kind: HeadDetached, Commit: line[:7]}
	if branch, ok := rebaseBranch(gitdir); ok {
		h.Kind, h.Branch = HeadRebasing, branch
	}
	return h
}

// rebaseBranch reports the branch a rebase in flight is replaying. Both
// backends leave a state dir beside HEAD; head-name holds the ref, or the
// literal "detached HEAD" when there is no branch, which is reported as a
// rebase without one rather than as a branch by that name.
func rebaseBranch(gitdir string) (string, bool) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		dir := filepath.Join(gitdir, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "head-name"))
		if err != nil {
			return "", true
		}
		if branch, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "refs/heads/"); ok {
			return branch, true
		}
		return "", true
	}
	return "", false
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// canon resolves symlinks so two spellings of one checkout compare equal.
func canon(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}
