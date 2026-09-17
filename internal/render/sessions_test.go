package render

import (
	"strings"
	"testing"
	"time"

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

func TestSessionLinesNarrowKeepsAge(t *testing.T) {
	p := NewPalette(false)
	s := &sessions.Session{
		Local: true, DirOK: true, Live: sessions.Live,
		MTime: time.Now().Add(-2 * time.Hour),
		Owner: "someone@example.com", OwnerState: sessions.OwnerHistory,
		// A checkout that has moved since: the widest label the row can
		// produce, live branch plus the observation in parentheses.
		Head: sessions.Head{Kind: sessions.HeadBranch, Branch: "develop"},
		Tail: sessions.Tail{AITitle: "a title", Branch: "a-longish-branch-name", Model: "claude-fable-5"},
	}
	for _, w := range []int{20, 30, 40, 120} {
		lines := p.SessionLines(s, "someone@example.com", false, w, time.Now().Unix())
		line1 := lines[0]
		if got := Cells(line1); got > w {
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

func TestCheckoutLabelPrefersLiveHead(t *testing.T) {
	on := func(b string) sessions.Head { return sessions.Head{Kind: sessions.HeadBranch, Branch: b} }
	main := func(h sessions.Head, observed string) sessions.Session {
		return sessions.Session{
			Local: true, RepoKey: "/dev/planlab", RepoRoot: "/dev/planlab",
			Head: h, Tail: sessions.Tail{Branch: observed},
		}
	}
	worktree := func(h sessions.Head, observed string) sessions.Session {
		return sessions.Session{
			Local: true, RepoKey: "/dev/planlab", RepoRoot: "/dev/.worktrees/main/feat/better-skill-subagents",
			Head: h, Tail: sessions.Tail{Branch: observed},
		}
	}
	cases := []struct {
		name string
		s    sessions.Session
		want string
	}{
		{"the reported bug: live branch leads, the session's own follows",
			main(on("develop"), "skill/baton-pointer-and-onboarding-order"),
			"develop (was skill/baton-pointer-and-onboarding-order)"},
		{"agreement says it once",
			main(on("main"), "main"), "main"},
		{"a worktree re-pointed since the session ran",
			worktree(on("docs/test-patterns-refresh"), "feat/better-skill-subagents"),
			"docs/test-patterns-refresh (was feat/better-skill-subagents)"},
		{"a worktree still on its branch leads with it, not the dir name",
			worktree(on("feat/better-skill-subagents"), "feat/better-skill-subagents"),
			"feat/better-skill-subagents"},
		{"a detached worktree falls back to the one name its path guarantees",
			worktree(sessions.Head{Kind: sessions.HeadDetached, Commit: "1b12251"}, "lane-b"),
			"better-skill-subagents"}, // filepath.Base, as the dir-name label always was
		{"a detached main checkout reports the state, never a name",
			main(sessions.Head{Kind: sessions.HeadDetached, Commit: "fdc9b62"}, "develop"),
			"detached@fdc9b62"},
		{"a rebase in flight is the story; no history tacked on",
			main(sessions.Head{Kind: sessions.HeadRebasing, Branch: "develop", Commit: "fdc9b62"}, "develop"),
			"develop (rebasing)"},
		{"a rebase with no branch to name",
			main(sessions.Head{Kind: sessions.HeadRebasing, Commit: "aaaaaaa"}, "topic"),
			"rebasing"},
		{"the vendor's literal HEAD is a detached state, not a branch",
			main(sessions.Head{}, "HEAD"), ""},
		{"a live branch outlives an observation that named nothing",
			main(on("main"), "HEAD"), "main"},
	}
	for _, c := range cases {
		if got := checkoutLabel(&c.s); got != c.want {
			t.Errorf("%s: checkoutLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCheckoutLabelNeverLeadsWithAStaleBranch(t *testing.T) {
	stale := sessions.Session{
		Local: true, RepoKey: "/dev/planlab", RepoRoot: "/dev/planlab",
		Head: sessions.Head{Kind: sessions.HeadBranch, Branch: "develop"},
		Tail: sessions.Tail{Branch: "skill/baton-pointer-and-onboarding-order"},
	}
	if got := checkoutLabel(&stale); !strings.HasPrefix(got, "develop") {
		t.Errorf("label %q leads with something other than the live branch", got)
	}
}

func TestCheckoutLabelNeverPassesHistoryOffAsCurrent(t *testing.T) {
	base := func(dirOK bool, kind sessions.HeadKind) sessions.Session {
		return sessions.Session{
			Local: true, DirOK: dirOK, RepoKey: "/dev/planlab", RepoRoot: "/dev/planlab",
			Head: sessions.Head{Kind: kind},
			Tail: sessions.Tail{Branch: "skill/baton-pointer-and-onboarding-order"},
		}
	}
	for _, kind := range []sessions.HeadKind{sessions.HeadUnreadable, sessions.HeadNone} {
		s := base(true, kind)
		got := checkoutLabel(&s)
		if got == "skill/baton-pointer-and-onboarding-order" {
			t.Errorf("kind %v: label %q is the remembered branch, unqualified", kind, got)
		}
		if !strings.Contains(got, "skill/baton-pointer-and-onboarding-order") {
			t.Errorf("kind %v: label %q dropped the one piece of evidence there was", kind, got)
		}
	}

	// A worktree dir name reads exactly like a branch, so it may not occupy
	// the branch slot when the checkout would not say what it is on — that is
	// the same claim-by-appearance the whole surface exists to stop.
	wt := base(true, sessions.HeadUnreadable)
	wt.RepoRoot = "/dev/.worktrees/main/feat/better-skill-subagents"
	if got := checkoutLabel(&wt); !strings.HasPrefix(got, "?") {
		t.Errorf("unreadable worktree label = %q, want the unknown to lead", got)
	}

	// Nothing remembered either: the row must still say it could not look,
	// rather than quietly becoming an ordinary project row.
	bare := base(true, sessions.HeadUnreadable)
	bare.Tail.Branch = ""
	if got := checkoutLabel(&bare); got != "?" {
		t.Errorf("unreadable checkout with no history = %q, want %q", got, "?")
	}
	// A row whose directory is gone cannot mislead about a destination — it
	// already carries ✗ dir gone — so its history stands unqualified.
	gone := base(false, sessions.HeadNone)
	gone.RepoKey, gone.RepoRoot = "", ""
	if got := checkoutLabel(&gone); got != "skill/baton-pointer-and-onboarding-order" {
		t.Errorf("dir-gone label = %q, want the bare observation", got)
	}
}
