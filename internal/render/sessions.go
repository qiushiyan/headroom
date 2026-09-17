package render

import (
	"path/filepath"
	"strings"

	"github.com/qiushiyan/headroom/internal/sessions"
)

func ProjectLabel(s *sessions.Session) string {
	switch {
	case s.RepoKey != "":
		return filepath.Base(s.RepoKey)
	case s.CWD != "":
		return filepath.Base(s.CWD)
	default:
		return filepath.Base(s.StoreDir)
	}
}

// checkoutLabel names a session's checkout the way a row has to read it:
// where enter lands you, as of this listing. Two different claims feed it —
// the live HEAD, read from the filesystem at collect time, and the branch the
// transcript observed at the session's last activity — and they age
// differently. The live one leads, because the first token of a row must
// never be a fossil; the observed one follows in parentheses when it
// disagrees, because that disagreement is the only thing distinguishing ten
// idle sessions whose checkout has since moved back to one shared branch.
func checkoutLabel(s *sessions.Session) string {
	was := s.Tail.ObservedBranch()
	if s.Head.Kind == sessions.HeadUnreadable {
		// The checkout is right there and would not say what it is on. Nothing
		// may take the branch slot then — not the remembered branch, and not a
		// worktree's dir name, which reads exactly like one. The unknown leads
		// and the observation stays behind it as history.
		if was != "" {
			return "? (was " + was + ")"
		}
		return "?"
	}
	if now := headLabel(s); now != "" {
		// The parenthetical claims the checkout has *moved*, so it needs a
		// live branch to have moved to. A detached HEAD, or a worktree named
		// only by its path, is not evidence of change — annotating those
		// would invent a story out of the absence of one.
		if s.Head.Kind == sessions.HeadBranch && was != "" && was != now {
			return now + " (was " + was + ")"
		}
		return now
	}
	if was == "" || !s.DirOK {
		// A row whose directory is gone leads with history because history is
		// all it has, and it already says so: ✗ dir gone. There is no
		// destination left for the label to be wrong about.
		return was
	}
	// A directory that is still there but sits in no repository at all: the
	// remembered branch is a fossil of a checkout that no longer exists, and
	// the row is still a destination, so it is marked the same way.
	return "? (was " + was + ")"
}

// headLabel spells the live HEAD. A branch names a checkout as precisely as
// its path does — git forbids two worktrees of one repo on the same branch —
// so it leads, and the worktree dir name is what a HEAD pointing at no branch
// falls back to. Detached is reported as the state it is, never as a name.
func headLabel(s *sessions.Session) string {
	if b := s.Head.Branch; b != "" {
		if s.Head.Kind == sessions.HeadRebasing {
			return b + " (rebasing)"
		}
		return b
	}
	if w := worktreeName(s); w != "" {
		return w
	}
	switch s.Head.Kind {
	case sessions.HeadRebasing:
		return "rebasing"
	case sessions.HeadDetached:
		return "detached@" + s.Head.Commit
	}
	return ""
}

// worktreeName is a linked worktree's dir name — the one piece of checkout
// identity that comes from the path and so cannot go stale.
func worktreeName(s *sessions.Session) string {
	if s.RepoRoot != "" && s.RepoRoot != s.RepoKey {
		return filepath.Base(s.RepoRoot)
	}
	return ""
}

func ownerTag(s *sessions.Session, current string) string {
	switch s.OwnerState {
	case sessions.OwnerMissing:
		return "owner gone→" + ShortAccount(current)
	case sessions.OwnerConflict:
		return "owner?→" + ShortAccount(current)
	case sessions.OwnerNone:
		return "→" + ShortAccount(current)
	default:
		return "@" + ShortAccount(s.Owner)
	}
}

func ShortAccount(name string) string {
	if name == "" {
		// No valid current account: `.current` failed strict resolution.
		// "?" over a name nobody chose.
		return "?"
	}
	local, _, _ := strings.Cut(name, "@")
	return local
}

// sessionLines renders one session as a two-line entry. The first line
// carries what muscle memory recognizes a session by — where it ran (branch
// or worktree locally, branch·project elsewhere), whether it is open right
// now, and the meta column: model, account, age. The second line, dimmed,
// is just the confirming title.
func (p Palette) SessionLines(s *sessions.Session, current string, selected bool, w int, now int64) []string {

	var marks []string
	if s.Live == sessions.Live {
		marks = append(marks, p.Grn+"● live"+p.Rst)
	}
	if s.Live == sessions.LiveUnknown {
		marks = append(marks, p.Yel+"● live?"+p.Rst)
	}
	if !s.DirOK {
		marks = append(marks, p.Red+"✗ dir gone"+p.Rst)
	}
	marksStr := strings.Join(marks, " ")

	marksCells := 0
	if marksStr != "" {
		marksCells = Cells(stripSGR(marksStr)) + 2
	}

	// The meta column budgets itself before the label flexes, and sheds from
	// its own left when the terminal is narrow — the model first, the account
	// second, the age never: the final Clip cuts lines from the right, and a
	// row without its timestamp is the one degradation this repo forbids.
	var meta []string
	if m := modelLabel(s.Tail.Model); m != "" {
		meta = append(meta, m)
	}
	meta = append(meta, ownerTag(s, current), Age(now-s.MTime.Unix()))
	metaAvail := max(w-2-4-2-marksCells, 2) // prefix, minimum label, gap
	for len(meta) > 1 && Cells(strings.Join(meta, " · ")) > metaAvail {
		meta = meta[1:]
	}
	right := strings.Join(meta, " · ")
	if Cells(right) > metaAvail {
		right = TrimCells(right, metaAvail)
	}

	prefix := "  "
	if selected {
		prefix = p.Bold + "▶ " + p.Rst
	}
	// Flexing label over a fixed right column. Cell arithmetic, not runes:
	// vendor text is the first place a CJK string meets framePrinter-style
	// layouts.
	label := Sanitize(primaryLabel(s))
	labelWidth := max(w-2-Cells(right)-2-marksCells, 4)
	if Cells(label) > labelWidth {
		label = TrimCells(label, labelWidth-1) + "…"
	}
	left := prefix + label
	if marksStr != "" {
		left += "  " + marksStr
	}
	pad := max(w-Cells(stripSGR(left))-Cells(right), 2)
	line1 := left + strings.Repeat(" ", pad) + p.Dim + right + p.Rst

	title := Sanitize(s.Tail.Title())
	if title == "" {
		title = "⟨untitled⟩"
	}
	titleWidth := max(w-4, 8)
	if Cells(title) > titleWidth {
		title = TrimCells(title, titleWidth-1) + "…"
	}
	line2 := "    " + p.Dim + title + p.Rst
	return []string{line1, line2}
}

// primaryLabel is a session's first-line identity — the checkout it resumes
// into, resolved by checkoutLabel, with the project appended as the
// disambiguator in the global section. Both sections resolve the checkout the
// same way — a worktree session must not change identity by scrolling past
// the section break.
func primaryLabel(s *sessions.Session) string {
	if s.Local {
		if l := checkoutLabel(s); l != "" {
			return l
		}
		return ProjectLabel(s)
	}
	proj := ProjectLabel(s)
	if l := checkoutLabel(s); l != "" && l != proj {
		return l + " · " + proj
	}
	return proj
}

// modelLabel compresses a vendor model id for the meta column:
// "claude-opus-4-5-20251101" → "opus-4.5", "claude-fable-5" → "fable-5".
// Unknown shapes pass through minus the claude- prefix — a new id must stay
// identifiable, never disappear.
func modelLabel(id string) string {
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "-")
	if parts[0] == "claude" {
		parts = parts[1:]
	}
	var name, ver []string
	for _, part := range parts {
		switch {
		case !allDigits(part):
			name = append(name, part)
		case len(part) == 8:
			// date stamp — precision the row doesn't need
		default:
			ver = append(ver, part)
		}
	}
	label := strings.Join(name, "-")
	if len(ver) > 0 {
		if label != "" {
			label += "-"
		}
		label += strings.Join(ver, ".")
	}
	return label
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// stripSGR removes escape sequences for width computation.
func stripSGR(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r >= '@' && r <= '~' && r != '[' {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (p Palette) SessionPreview(s *sessions.Session, w int) []string {
	var out []string
	add := func(prefix, text string) {
		if text == "" {
			return
		}
		text = Sanitize(text)
		width := max(w-8, 10)
		for i, line := 0, ""; text != "" && i < previewCap; i++ {
			line = TrimCells(text, width)
			text = text[len(line):]
			out = append(out, "    "+p.Dim+prefix+line+p.Rst)
			prefix = "  "
		}
	}
	prompt := s.Tail.LastPrompt
	if prompt == "" {
		prompt = s.Tail.LastUser
	}
	add("> ", prompt)
	add("< ", s.Tail.LastReply)
	if len(out) == 0 {
		out = append(out, "    "+p.Dim+"(no preview)"+p.Rst)
	}
	return out
}

const previewCap = 3
