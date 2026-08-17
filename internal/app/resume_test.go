package app

import (
	"strings"
	"time"

	"testing"

	"github.com/qiushiyan/headroom/internal/accounts"
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
		// A checkout that has moved since: the widest label the row can
		// produce, live branch plus the observation in parentheses.
		Head: sessions.Head{Kind: sessions.HeadBranch, Branch: "develop"},
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

// Every session here has an unreadable HEAD (the zero Head), so these are
// the fallback shapes: a dir that is gone, a checkout whose HEAD no longer
// parses. With nothing live to read the transcript's observation is all
// there is, and it must still produce the row it always did.
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

// The two fail-closed rules of resume routing. Pinned red against the
// pre-review code, which fell back to the primary in both cases — minting a
// launch decision no evidence had chosen.
func TestResumeAccountFailsClosedOnInvalidCurrent(t *testing.T) {
	ui := &resumeUI{current: "", accts: []accounts.Account{{Name: "qiushi"}}}
	if a, ok := ui.resumeAccount(&sessions.Session{}, false); ok {
		t.Errorf("ownerless session with no valid current resumed on %q — a decision minted from corrupt routing state", a.Name)
	}
}

func TestResumeAccountDeletedOwnerFallsToCurrentNeverPrimary(t *testing.T) {
	ui := &resumeUI{
		current: "b@x.com",
		accts:   []accounts.Account{{Name: "qiushi"}, {ConfigDir: "/r/b@x.com", Name: "b@x.com"}},
	}
	a, ok := ui.resumeAccount(&sessions.Session{Owner: "gone@x.com"}, false)
	if !ok || a.Name != "b@x.com" {
		t.Errorf("deleted owner resumed on (%q, %v) — degraded attribution falls back to current, never primary", a.Name, ok)
	}
}

// The rule the branch label exists to enforce: what a row leads with is the
// live HEAD, read this listing — never the transcript's observation, which
// was true of the session's last activity and goes stale the moment anyone
// checks out. The observation survives as the parenthetical, because ten
// idle sessions whose checkout has since moved back to `develop` are
// otherwise indistinguishable.
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

// A row must never lead with a branch the checkout has left — the whole
// point — and the annotation must never be minted without a live branch to
// justify it.
func TestCheckoutLabelNeverLeadsWithAStaleBranch(t *testing.T) {
	stale := sessions.Session{
		Local: true, RepoKey: "/dev/planlab", RepoRoot: "/dev/planlab",
		Head: sessions.Head{Kind: sessions.HeadBranch, Branch: "develop"},
		Tail: sessions.Tail{Branch: "skill/baton-pointer-and-onboarding-order"},
	}
	if got := checkoutLabel(&stale); !strings.HasPrefix(got, "develop") {
		t.Errorf("label %q leads with something other than the live branch", got)
	}
	unreadable := stale
	unreadable.Head = sessions.Head{}
	if got := checkoutLabel(&unreadable); strings.Contains(got, "was") {
		t.Errorf("label %q invented a move with no live branch to move to", got)
	}
}

// Search has to find a session by either branch: the one it ran on, which is
// what you remember, and the one its checkout is on now, which is what you
// see in the row.
func TestMatchesFindsEitherBranch(t *testing.T) {
	s := &sessions.Session{
		Head: sessions.Head{Kind: sessions.HeadBranch, Branch: "develop"},
		Tail: sessions.Tail{Branch: "skill/baton-pointer-and-onboarding-order"},
	}
	for _, q := range []string{"develop", "baton-pointer"} {
		if !matches(s, q) {
			t.Errorf("query %q missed the session", q)
		}
	}
}
