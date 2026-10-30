package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

// RepoKey answers one thing for every spelling of a place in a repository: a
// worktree, a directory inside one, and a symlink that only resolves into one,
// which has no .git above its own spelling.
func TestRepoKeyIsTheMainCheckoutHoweverThePathIsSpelled(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(base, "itell")
	worktree := filepath.Join(base, "trees", "itell", "feat")
	neighbour := filepath.Join(base, "itell-cms")
	plain := filepath.Join(base, "plain", "sub")
	for _, dir := range []string{filepath.Join(main, ".git", "worktrees", "feat"), filepath.Join(main, "apps"), filepath.Join(worktree, "apps"), filepath.Join(neighbour, ".git"), plain} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(main, ".git", "HEAD"):                      "ref: refs/heads/main\n",
		filepath.Join(main, ".git", "worktrees", "feat", "HEAD"): "ref: refs/heads/feat\n",
		filepath.Join(neighbour, ".git", "HEAD"):                 "ref: refs/heads/main\n",
		filepath.Join(worktree, ".git"):                          "gitdir: " + filepath.Join(main, ".git", "worktrees", "feat") + "\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(worktree, "apps"), link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ dir, want string }{
		{main, main},
		{filepath.Join(main, "apps"), main},
		{worktree, main},
		{filepath.Join(worktree, "apps"), main},
		{link, main},
		{neighbour, neighbour},
		{plain, plain},
		{filepath.Join(base, "gone"), ""},
		{"relative/path", ""},
		{"", ""},
	} {
		if got := RepoKey(c.dir); got != c.want {
			t.Errorf("RepoKey(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}
