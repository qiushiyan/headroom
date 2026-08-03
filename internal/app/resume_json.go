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
	Branch      string `json:"branch,omitempty"`
	Local       bool   `json:"local"`
	ModifiedAt  string `json:"modified_at"` // RFC3339 UTC
	SizeBytes   int64  `json:"size_bytes"`
	Owner       string `json:"owner,omitempty"`
	OwnerSource string `json:"owner_source"`        // live|rehome|history|missing|conflict|none
	Live        string `json:"live"`                // yes|no|unknown
	BadLines    int    `json:"bad_lines,omitempty"` // tail lines that failed the contract — drift, surfaced
	Path        string `json:"path"`
}

func runResumeJSON(cfg config.Config) int {
	listing, _, _, current := collectSessions(cfg)
	doc := resumeDoc{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
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
			Branch:      s.Tail.Branch,
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
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(os.Stderr, "headroom resume --json: %v\n", err)
		return 1
	}
	return 0
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
