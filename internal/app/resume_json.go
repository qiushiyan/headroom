package app

// The session listing for machines: the same collector as the picker,
// serialized instead of drawn — no TTY, no /dev/tty, no interaction. Owner
// provenance and degradation travel as explicit state strings so a consumer
// can tell "no owner evidence" from "owner's account was deleted" without
// re-deriving either. Field changes bump "schema".

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
)

type resumeDoc struct {
	Schema      int           `json:"schema"`
	GeneratedAt string        `json:"generated_at"` // RFC3339 UTC
	Current     string        `json:"current"`      // account new sessions target
	LocalKey    string        `json:"local_key"`    // what "local" groups on
	Sessions    []jsonSession `json:"sessions"`
}

type jsonSession struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	TitleSource string `json:"title_source"` // custom|ai|last_prompt|last_user|none
	ProjectDir  string `json:"project_dir,omitempty"`
	DirOK       bool   `json:"dir_ok"`
	Repo        string `json:"repo,omitempty"`
	Checkout    string `json:"checkout,omitempty"` // worktree/checkout root
	// Branch is what Checkout is on *now*, read at collect time; HeadState
	// says whether there was a branch to read. BranchAtLastActivity is the
	// transcript's own observation, true only of when the session last ran.
	// Selecting on the wrong one of these is the machine version of the bug
	// this pair exists to prevent — they diverge the moment anyone checks out.
	Branch               string `json:"branch,omitempty"`
	HeadState            string `json:"head_state"`            // branch|detached|rebasing|unreadable|none
	HeadCommit           string `json:"head_commit,omitempty"` // abbreviated sha, when detached
	BranchAtLastActivity string `json:"branch_at_last_activity,omitempty"`
	Model                string `json:"model,omitempty"` // newest assistant model id, verbatim
	Local                bool   `json:"local"`
	ModifiedAt           string `json:"modified_at"` // RFC3339 UTC
	SizeBytes            int64  `json:"size_bytes"`
	Owner                string `json:"owner,omitempty"`
	OwnerSource          string `json:"owner_source"`        // live|rehome|history|missing|conflict|none
	Live                 string `json:"live"`                // yes|no|unknown
	BadLines             int    `json:"bad_lines,omitempty"` // tail lines that failed the contract — drift, surfaced
	Path                 string `json:"path"`
}

func runSessionsJSON(cfg config.Config) int {
	listing, _, _, current := collectSessions(cfg, state.Open(cfg.AccountsRoot))
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sessionsDoc(listing, current, time.Now())); err != nil {
		fmt.Fprintf(os.Stderr, "headroom sessions --json: %v\n", err)
		return 1
	}
	return 0
}

// sessionsDoc is the whole wire contract as a pure function of the listing —
// no config, no store, no stdout — so the mapping every consumer depends on
// can be asserted directly rather than inferred from a run.
func sessionsDoc(listing sessions.Listing, current string, now time.Time) resumeDoc {
	doc := resumeDoc{
		Schema:      3, // 3: "branch" is now the live HEAD; observation moved to "branch_at_last_activity"
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Current:     current,
		LocalKey:    listing.LocalKey,
	}
	for _, s := range listing.Sessions {
		doc.Sessions = append(doc.Sessions, jsonSession{
			ID:          s.ID,
			Title:       s.Tail.Title(),
			TitleSource: titleSource(s),
			ProjectDir:  s.CWD,
			DirOK:       s.DirOK,
			Repo:        s.RepoKey,
			Checkout:    s.RepoRoot,
			Branch:      s.Head.Branch,
			HeadState:   headState(s.Head.Kind),
			HeadCommit:  s.Head.Commit,

			BranchAtLastActivity: s.Tail.ObservedBranch(),

			Model:       s.Tail.Model,
			Local:       s.Local,
			ModifiedAt:  s.MTime.UTC().Format(time.RFC3339),
			SizeBytes:   s.Size,
			Owner:       s.Owner,
			OwnerSource: ownerSource(s.OwnerState),
			Live:        liveString(s.Live),
			BadLines:    s.Tail.Bad,
			Path:        s.Path,
		})
	}
	return doc
}

func titleSource(s *sessions.Session) string {
	switch {
	case s.Tail.CustomTitle != "":
		return "custom"
	case s.Tail.AITitle != "":
		return "ai"
	case s.Tail.LastPrompt != "":
		return "last_prompt"
	case s.Tail.LastUser != "":
		return "last_user"
	default:
		return "none"
	}
}

func ownerSource(st sessions.OwnerState) string {
	switch st {
	case sessions.OwnerLive:
		return "live"
	case sessions.OwnerRehome:
		return "rehome"
	case sessions.OwnerHistory:
		return "history"
	case sessions.OwnerMissing:
		return "missing"
	case sessions.OwnerConflict:
		return "conflict"
	default:
		return "none"
	}
}

func liveString(l sessions.LiveState) string {
	switch l {
	case sessions.Live:
		return "yes"
	case sessions.LiveUnknown:
		return "unknown"
	default:
		return "no"
	}
}

// headState is the wire spelling of a checkout's live HEAD, and it keeps the
// three answers apart that a consumer has to act on differently: on a branch,
// not on one, and no answer available. "none" means there is no checkout at
// this path to have a branch; "unreadable" means there is one and its HEAD no
// longer says — the same absent-versus-broken distinction the tag vocabulary
// draws for vendor fields, and the one that decides whether
// branch_at_last_activity may be shown as though it were current. It may not:
// it is history under either.
func headState(k sessions.HeadKind) string {
	switch k {
	case sessions.HeadBranch:
		return "branch"
	case sessions.HeadDetached:
		return "detached"
	case sessions.HeadRebasing:
		return "rebasing"
	case sessions.HeadUnreadable:
		return "unreadable"
	default:
		return "none"
	}
}
