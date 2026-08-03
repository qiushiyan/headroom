package app

import (
	"strings"
	"time"

	"testing"

	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
)

func TestModelLabel(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"claude-fable-5":             "fable-5",
		"claude-opus-5":              "opus-5",
		"claude-opus-4-5-20251101":   "opus-4.5",
		"claude-haiku-4-5-20251001":  "haiku-4.5",
		"claude-3-5-sonnet-20241022": "sonnet-3.5",
		"sonnet":                     "sonnet", // unknown shape passes through, never disappears
	}
	for in, want := range cases {
		if got := modelLabel(in); got != want {
			t.Errorf("modelLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// On a narrow terminal the meta column must shed from its left — model
// first, then account — never lose the age: the final Clip in draw cuts
// from the right, and a row without its timestamp violates the house rule
// that observations always carry one.
func TestSessionLinesNarrowKeepsAge(t *testing.T) {
	ui := &resumeUI{p: render.NewPalette(false), current: "someone@example.com"}
	s := &sessions.Session{
		Local: true, DirOK: true, Live: sessions.Live,
		MTime: time.Now().Add(-2 * time.Hour),
		Owner: "someone@example.com", OwnerState: sessions.OwnerHistory,
		Tail: sessions.Tail{AITitle: "a title", Branch: "a-longish-branch-name", Model: "claude-fable-5"},
	}
	for _, w := range []int{20, 30, 40, 120} {
		lines := ui.sessionLines(s, false, w, time.Now().Unix())
		line1 := lines[0]
		if got := render.Cells(line1); got > w {
			t.Errorf("w=%d: first line is %d cells — Clip would eat the right column", w, got)
		}
		if !strings.HasSuffix(line1, "2h") {
			t.Errorf("w=%d: age missing from %q", w, line1)
		}
	}
}

func TestPrimaryLabel(t *testing.T) {
	branch := func(b string) sessions.Tail { return sessions.Tail{Branch: b} }
	cases := []struct {
		name string
		s    sessions.Session
		want string
	}{
		{"local main checkout shows branch",
			sessions.Session{Local: true, RepoKey: "/dev/headroom", RepoRoot: "/dev/headroom", Tail: branch("main")},
			"main"},
		{"local worktree shows its dir name",
			sessions.Session{Local: true, RepoKey: "/dev/headroom", RepoRoot: "/dev/.worktrees/headroom/session-picker", Tail: branch("session-picker")},
			"session-picker"},
		{"local non-git falls back to the dir",
			sessions.Session{Local: true, CWD: "/Users/q/training"},
			"training"},
		{"global worktree keeps its checkout identity",
			sessions.Session{RepoKey: "/dev/headroom", RepoRoot: "/dev/.worktrees/headroom/session-picker", Tail: branch("other-branch")},
			"session-picker · headroom"},
		{"global leads with the branch, project disambiguates",
			sessions.Session{RepoKey: "/dev/planlab", RepoRoot: "/dev/planlab", CWD: "/dev/planlab", Tail: branch("chart-axis")},
			"chart-axis · planlab"},
		{"global without a branch is the project",
			sessions.Session{CWD: "/Users/q/Documents/personal"},
			"personal"},
	}
	for _, c := range cases {
		if got := primaryLabel(&c.s); got != c.want {
			t.Errorf("%s: primaryLabel = %q, want %q", c.name, got, c.want)
		}
	}
}
