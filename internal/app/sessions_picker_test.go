package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
)

// The two fail-closed rules of resume routing. Pinned red against the
// pre-review code, which fell back to the primary in both cases — minting a
// launch decision no evidence had chosen.
func TestResumeAccountFailsClosedOnInvalidCurrent(t *testing.T) {
	ui := &sessionActions{set: accounts.Set{Accounts: []accounts.Account{{Name: "qiushi"}}}}
	if intent, ok := ui.resumeIntent(&sessions.Session{}, false); ok {
		t.Errorf("ownerless session with no valid current resumed on %q — a decision minted from corrupt routing state", intent.Account)
	}
	if intent, ok := ui.resumeIntent(&sessions.Session{Owner: "qiushi"}, true); ok {
		t.Errorf("the override with no valid current chose %q", intent.Account)
	}
}

func TestResumeAccountDeletedOwnerFallsToCurrentNeverPrimary(t *testing.T) {
	ui := &sessionActions{
		routing: accounts.Routing{Mode: "pinned", Current: "b@x.com"},
		set:     accounts.Set{Accounts: []accounts.Account{{Name: "qiushi"}, {ConfigDir: "/r/b@x.com", Name: "b@x.com"}}},
	}
	intent, ok := ui.resumeIntent(&sessions.Session{Owner: "gone@x.com"}, false)
	if !ok || intent.Kind != placement.Forced || intent.Account != "b@x.com" {
		t.Errorf("deleted owner resumed on (%+v, %v) — degraded attribution falls back to current, never primary", intent, ok)
	}
	// An owner that exists is followed; the override goes to current instead.
	if intent, _ := ui.resumeIntent(&sessions.Session{Owner: "qiushi"}, false); intent.Account != "qiushi" || intent.Reason != placement.ReasonOwner {
		t.Errorf("an existing owner: %+v", intent)
	}
	if intent, _ := ui.resumeIntent(&sessions.Session{Owner: "qiushi"}, true); intent.Account != "b@x.com" {
		t.Errorf("the override: %+v", intent)
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

// The wire contract, asserted rather than inferred: which of the two branch
// facts each field carries. Reverting either mapping used to leave the whole
// suite green, which made the field a consumer depends on free to drift.
func TestSessionsDocCarriesBothBranchFacts(t *testing.T) {
	moved := &sessions.Session{
		ID: "s-moved", CWD: "/dev/planlab/main", DirOK: true,
		RepoKey: "/dev/planlab/main", RepoRoot: "/dev/planlab/main",
		Head: sessions.Head{Kind: sessions.HeadBranch, Branch: "develop"},
		Tail: sessions.Tail{Branch: "skill/baton-pointer-and-onboarding-order"},
	}
	detached := &sessions.Session{
		ID: "s-detached", CWD: "/dev/headroom", DirOK: true,
		Head: sessions.Head{Kind: sessions.HeadDetached, Commit: "fdc9b62"},
		Tail: sessions.Tail{Branch: "HEAD"},
	}
	husk := &sessions.Session{
		ID: "s-husk", CWD: "/dev/itell", DirOK: true,
		Head: sessions.Head{Kind: sessions.HeadUnreadable},
		Tail: sessions.Tail{Branch: "main"},
	}
	doc := sessionsDoc(sessions.Listing{Sessions: []*sessions.Session{moved, detached, husk}},
		"a@x.com", time.Now())

	if doc.Schema != 3 {
		t.Errorf("schema = %d, want 3 — the branch fields changed meaning", doc.Schema)
	}
	by := map[string]jsonSession{}
	for _, s := range doc.Sessions {
		by[s.ID] = s
	}

	// "branch" is the checkout's now; the observation lives under its own name.
	if got := by["s-moved"]; got.Branch != "develop" ||
		got.BranchAtLastActivity != "skill/baton-pointer-and-onboarding-order" ||
		got.HeadState != "branch" {
		t.Errorf("moved checkout: %+v", got)
	}
	// Detached publishes the state and the commit, and never a branch — the
	// vendor's literal "HEAD" is not one, on this surface either.
	if got := by["s-detached"]; got.Branch != "" || got.BranchAtLastActivity != "" ||
		got.HeadState != "detached" || got.HeadCommit != "fdc9b62" {
		t.Errorf("detached checkout: %+v", got)
	}
	// "unreadable" is what stops a consumer reading the observation as current.
	if got := by["s-husk"]; got.Branch != "" || got.HeadState != "unreadable" ||
		got.BranchAtLastActivity != "main" {
		t.Errorf("unreadable HEAD: %+v", got)
	}
}

// The --json enums are wire spellings a consumer switches on — this is their
// one pin: a session per owner source, per live state and per title source,
// each read back by id. Swapping any two spellings used to leave the suite
// green.
func TestSessionsDocSpellsEveryState(t *testing.T) {
	owners := map[sessions.OwnerState]string{
		sessions.OwnerNone: "none", sessions.OwnerLive: "live", sessions.OwnerRehome: "rehome",
		sessions.OwnerHistory: "history", sessions.OwnerMissing: "missing", sessions.OwnerConflict: "conflict",
	}
	lives := map[sessions.LiveState]string{sessions.NotLive: "no", sessions.Live: "yes", sessions.LiveUnknown: "unknown"}
	titles := []struct {
		tail          sessions.Tail
		title, source string
	}{
		{sessions.Tail{CustomTitle: "renamed", AITitle: "generated"}, "renamed", "custom"},
		{sessions.Tail{AITitle: "generated", LastPrompt: "prompt"}, "generated", "ai"},
		{sessions.Tail{LastPrompt: "prompt", LastUser: "message"}, "prompt", "last_prompt"},
		{sessions.Tail{LastUser: "message"}, "message", "last_user"},
		{sessions.Tail{}, "", "none"},
	}

	var listing sessions.Listing
	add := func(id string, s sessions.Session) {
		s.ID = id
		listing.Sessions = append(listing.Sessions, &s)
	}
	for st := range owners {
		add(fmt.Sprintf("owner-%d", st), sessions.Session{OwnerState: st})
	}
	for st := range lives {
		add(fmt.Sprintf("live-%d", st), sessions.Session{Live: st})
	}
	for i, c := range titles {
		add(fmt.Sprintf("title-%d", i), sessions.Session{Tail: c.tail})
	}
	by := map[string]jsonSession{}
	for _, s := range sessionsDoc(listing, "", time.Now()).Sessions {
		by[s.ID] = s
	}

	for st, want := range owners {
		if got := by[fmt.Sprintf("owner-%d", st)].OwnerSource; got != want {
			t.Errorf("owner state %d: owner_source %q, want %q", st, got, want)
		}
	}
	for st, want := range lives {
		if got := by[fmt.Sprintf("live-%d", st)].Live; got != want {
			t.Errorf("live state %d: live %q, want %q", st, got, want)
		}
	}
	for i, c := range titles {
		if got := by[fmt.Sprintf("title-%d", i)]; got.Title != c.title || got.TitleSource != c.source {
			t.Errorf("title chain %+v: (%q, %q), want (%q, %q)", c.tail, got.Title, got.TitleSource, c.title, c.source)
		}
	}
}
