// Package sessions reads the machine-global Claude Code session store —
// transcripts under ~/.claude/projects — plus the per-account evidence of
// which account drove each session: prompt history, the live-session
// registry, and headroom's own .owners re-home record. It is the one reader
// of these vendor document types, shared by the resume picker, `--json` and
// `check`, per the house one-parser rule. Everything here is reverse-
// engineered and perishable; parsing degrades per field and tags what it
// couldn't read instead of dropping a session.
package sessions

import (
	"bytes"
	"encoding/json"
	"strings"
)

// TailBudget is how many bytes from the end of a transcript the metadata
// parser reads. Measured against the live store: the farthest title record
// sits 33KB from EOF (p90 28KB), so 64KB covers every transcript with margin.
// `check` asserts this stays true — a transcript whose title drifts beyond
// the budget is how "opens instantly" would silently become "shows untitled".
const TailBudget = 64 << 10

// Munge maps a session cwd to Claude Code's store-directory name for it:
// every '/' and '.' becomes '-'. Lossy by construction — never invert it;
// its one legitimate use is *verifying* a recorded cwd against the dir a
// transcript actually sits in.
func Munge(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '.' {
			return '-'
		}
		return r
	}, cwd)
}

// Tail is what the metadata pass extracts from a transcript's final bytes.
// Later records override earlier ones throughout: every field is "the newest
// one seen".
type Tail struct {
	ID          string // sessionId carried by records
	CustomTitle string // user rename (custom-title record) — outranks AITitle
	AITitle     string
	LastPrompt  string // the vendor's own "newest real prompt" record
	LastUser    string // newest plain user message — preview fallback
	LastReply   string // newest assistant text
	CWD         string // newest cwd whose munge equals the store dir name
	Branch      string // display hint only, never repo identity
	Model       string // newest assistant record's model id, verbatim

	Lines int // complete lines parsed
	Bad   int // complete lines that failed to parse — drift, tagged not hidden
}

// Title resolves the display title chain: rename beats generated beats the
// newest prompt. Empty means genuinely untitled — the caller renders that
// visibly, never as a blank row that reads like an empty session.
func (t Tail) Title() string {
	switch {
	case t.CustomTitle != "":
		return t.CustomTitle
	case t.AITitle != "":
		return t.AITitle
	case t.LastPrompt != "":
		return t.LastPrompt
	default:
		return t.LastUser
	}
}

// ObservedBranch is the branch this session was last seen on, or "" when the
// transcript names none. The vendor spells a detached HEAD as the literal
// "HEAD", which is a state and not a branch, so it is not one this can report
// — every surface asks here rather than reading Branch, so the picker and
// --json cannot disagree about what counts as a branch.
func (t Tail) ObservedBranch() string {
	if t.Branch == "HEAD" {
		return ""
	}
	return t.Branch
}

// tailRec is the union of every record field the tail pass reads. One decode
// shape for all record types: absent fields stay zero and the type switch
// below decides what matters.
type tailRec struct {
	Type         string  `json:"type"`
	SessionID    string  `json:"sessionId"`
	CustomTitle  string  `json:"customTitle"`
	AITitle      string  `json:"aiTitle"`
	LastPrompt   *string `json:"lastPrompt"`
	RelocatedCwd string  `json:"relocatedCwd"`
	CWD          string  `json:"cwd"`
	GitBranch    string  `json:"gitBranch"`
	IsMeta       bool    `json:"isMeta"`
	Message      *struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// ParseTail extracts session metadata from the final bytes of a transcript.
// When data is a mid-file suffix (whole=false), everything before its first
// newline is a partial line and is skipped; a torn final line is skipped in
// either case — a transcript being appended to mid-read must degrade a
// field, never invent one.
//
// storeDirName anchors the cd target: a transcript's cwd values can vary
// across its life (worktree relocations, in-repo subdirs), and the one that
// resolves `claude --resume` is the one the store dir was named after. So
// CWD keeps the newest cwd/relocatedCwd value whose Munge equals the dir
// name — verification against the store, never de-munging of it.
func ParseTail(data []byte, storeDirName string, whole bool) Tail {
	var t Tail
	if !whole {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return t
		}
		data = data[i+1:]
	}
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break // torn final line: skip, not drift
		}
		line := data[:nl]
		data = data[nl+1:]
		if len(line) == 0 {
			continue
		}
		t.Lines++
		var r tailRec
		if json.Unmarshal(line, &r) != nil {
			t.Bad++
			continue
		}
		if r.SessionID != "" {
			t.ID = r.SessionID
		}
		if cwd := r.RelocatedCwd; cwd != "" && Munge(cwd) == storeDirName {
			t.CWD = cwd
		}
		if r.CWD != "" && Munge(r.CWD) == storeDirName {
			t.CWD = r.CWD
		}
		if r.GitBranch != "" {
			t.Branch = r.GitBranch
		}
		switch r.Type {
		case "custom-title":
			t.CustomTitle = r.CustomTitle
		case "ai-title":
			t.AITitle = r.AITitle
		case "last-prompt":
			// The field is sometimes absent entirely ({leafUuid,sessionId,
			// type} shape in the live store); a nil pointer must not erase a
			// value an earlier record carried.
			if r.LastPrompt != nil {
				t.LastPrompt = *r.LastPrompt
			}
		case "user":
			if r.Message != nil && !r.IsMeta {
				if s := stringContent(r.Message.Content); s != "" {
					t.LastUser = s
				}
			}
		case "assistant":
			if r.Message != nil {
				// "<synthetic>" is the vendor's placeholder on error records —
				// a marker, not a model that drove anything. Tool-use-only
				// records still name the model, so this doesn't gate on text.
				if r.Message.Model != "" && r.Message.Model != "<synthetic>" {
					t.Model = r.Message.Model
				}
				if s := textBlocks(r.Message.Content); s != "" {
					t.LastReply = s
				}
			}
		}
	}
	return t
}

// stringContent reads a user message body when it is a plain string — the
// shape of a typed prompt. Structured content (tool results) is not a prompt.
func stringContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// textBlocks concatenates the text blocks of an assistant message.
func textBlocks(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
