package sessions

import (
	"errors"
	"io/fs"
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
//
// The first two are the house distinction between absent and broken, and they
// are not interchangeable. HeadNone says there is no checkout here to have a
// branch, so a caller has nothing to render and nothing to warn about;
// HeadUnreadable says there *is* one and we could not learn what it is on,
// which is the case where showing a remembered branch would quietly pass
// history off as the present. Collapsing them leaves callers unable to tell
// those apart, which is the whole bug this field exists to prevent.
type HeadKind uint8

const (
	HeadNone       HeadKind = iota // no checkout here: no HEAD to read
	HeadUnreadable                 // a checkout whose HEAD is unreadable or no longer parses
	HeadBranch                     // on a named branch
	HeadDetached                   // at a bare commit — sha checkout, bisect
	HeadRebasing                   // detached with a rebase in flight
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
	// A .git that names no git directory is a husk, not a checkout — the live
	// store has one holding nothing but info/exclude, several directories
	// below the repository it belongs to. Stopping there would key sessions on
	// a path git does not consider a checkout, so the walk keeps climbing and
	// only settles for the husk when nothing above it is real.
	var husk repoInfo
	for d := dir; ; {
		if info, ok := checkoutAt(d); ok {
			return info
		} else if info.Root != "" && husk.Root == "" {
			husk = info
		}
		parent := filepath.Dir(d)
		if parent == d {
			return husk
		}
		d = parent
	}
}

// checkoutAt reads the .git at dir, if there is one, and reports whether it
// names a real checkout — which is decided by its git directory holding a
// HEAD, the same evidence that tells a husk from the genuine article.
func checkoutAt(dir string) (repoInfo, bool) {
	gitPath := filepath.Join(dir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		return repoInfo{}, false
	}
	info := repoInfo{Key: canon(dir), Root: dir}
	var gitdir string
	switch {
	case fi.IsDir():
		gitdir = gitPath
	case fi.Mode().IsRegular():
		var ok bool
		if gitdir, ok = gitFileDir(gitPath, dir); !ok {
			// A .git file we cannot follow: this may still be the checkout,
			// but nothing here can name its branch or its main repository.
			return info, false
		}
		if main, _, found := strings.Cut(gitdir, "/.git/worktrees/"); found {
			info.Key = canon(main)
		}
		// Any other shape (submodule, future layout) has no honest main
		// checkout to name, so Key stays this dir — but its gitdir still
		// holds a readable HEAD.
	default:
		return repoInfo{}, false
	}
	info.Head = readHead(gitdir)
	return info, info.Head.Kind != HeadNone
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
// A missing HEAD is HeadNone — there is no checkout at this path, which is
// also how a .git husk is told from a real git directory. Every other failure
// is HeadUnreadable: the checkout is real and its HEAD no longer says
// anything, and a caller must be able to say so rather than fall back to a
// remembered branch as though it were current.
func readHead(gitdir string) Head {
	data, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Head{Kind: HeadNone}
		}
		return Head{Kind: HeadUnreadable}
	}
	line := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(line, "ref:"); ok {
		branch, ok := strings.CutPrefix(strings.TrimSpace(ref), "refs/heads/")
		if !ok || branch == "" {
			// A symref outside refs/heads is not a branch this can name.
			return Head{Kind: HeadUnreadable}
		}
		return Head{Kind: HeadBranch, Branch: branch}
	}
	if !isHex(line) || len(line) < 7 {
		return Head{Kind: HeadUnreadable}
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
