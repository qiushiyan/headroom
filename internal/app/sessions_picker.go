package app

// The session picker owns selection, modes and terminal drawing. The command
// that opens it is sessions.go; collection, presentation and committed
// actions belong to sessions, render and sessionActions respectively.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/tui"
)

// shown is the name the picker prints for where new sessions go: the current
// account, or the word for there being none because launches choose.
func (actions *sessionActions) shown() string {
	if actions.auto() {
		return accounts.AutoWord
	}
	return actions.routing.Current
}

// resumeUI is the picker's whole mutable state. One mode value at a time —
// pending-delete, confirm, rename and search each replace it wholesale, so
// "renaming while a delete is pending" is unrepresentable.
type resumeUI struct {
	sessionActions
	t       *tui.Terminal
	p       render.Palette
	listing sessions.Listing

	rows    []*sessions.Session // visible, filtered, local-then-global
	sel     int
	top     int // viewport scroll offset (row index space)
	query   string
	preview bool   // expanded preview under the selected row
	message string // one-line feedback, cleared on next keypress

	mode     uiMode
	deadline time.Time    // pending-delete expiry
	edit     tui.LineEdit // rename/search text
}

type uiMode int

const (
	modeNormal uiMode = iota
	modePendingD
	modeConfirmD
	modeRename
	modeSearch
)

const pendingDTimeout = 800 * time.Millisecond

// run draws the listing and applies keys until one ends the picker: a
// cancel, or a commit that leaves it (a successful resume never returns).
func (ui *resumeUI) run() int {
	ui.refilter("")
	ui.draw()

	for {
		var timeout <-chan time.Time
		if ui.mode == modePendingD {
			timeout = time.After(time.Until(ui.deadline))
		}
		select {
		case <-timeout:
			ui.mode = modeNormal
			ui.draw()
		case k := <-ui.t.Events():
			done, code := ui.handle(k)
			if done {
				return code
			}
			ui.draw()
		}
	}
}

// handle applies one key; (true, code) ends the session.
func (ui *resumeUI) handle(k tui.Key) (bool, int) {
	ui.message = ""
	switch ui.mode {
	case modeSearch:
		if ui.edit.Handle(k) {
			ui.refilter(ui.edit.String())
			return false, 0
		}
		switch k.Kind {
		case tui.KeyEnter:
			ui.mode = modeNormal // keep the filter
		case tui.KeyEsc:
			ui.mode = modeNormal
			ui.edit.SetString("")
			ui.refilter("")
		}
		return false, 0

	case modeRename:
		if ui.edit.Handle(k) {
			return false, 0
		}
		switch k.Kind {
		case tui.KeyEnter:
			ui.commitRename()
			ui.mode = modeNormal
		case tui.KeyEsc:
			ui.mode = modeNormal
		}
		return false, 0

	case modeConfirmD:
		switch {
		case k == tui.Key{Kind: tui.KeyRune, Rune: 'y'}:
			ui.commitDelete()
			ui.mode = modeNormal
		default:
			ui.mode = modeNormal
			ui.message = "delete cancelled"
		}
		return false, 0

	case modePendingD:
		ui.mode = modeNormal
		if k == (tui.Key{Kind: tui.KeyRune, Rune: 'd'}) && time.Now().Before(ui.deadline) {
			if s := ui.selected(); s != nil {
				if err := ui.mutable(s, errOpenDelete); err != nil {
					ui.message = err.Error()
				} else {
					ui.mode = modeConfirmD
				}
			}
			return false, 0
		}
		// Any other key falls through to normal handling of that key.
	}

	switch {
	case isCancelKey(k):
		ui.t.Close()
		return true, 1
	case k.Kind == tui.KeyUp || k == tui.Key{Kind: tui.KeyRune, Rune: 'k'}:
		ui.move(-1)
	case k.Kind == tui.KeyDown || k == tui.Key{Kind: tui.KeyRune, Rune: 'j'}:
		ui.move(1)
	case k == tui.Key{Kind: tui.KeyRune, Rune: 'g'} || k.Kind == tui.KeyHome:
		ui.sel = 0
	case k == tui.Key{Kind: tui.KeyRune, Rune: 'G'} || k.Kind == tui.KeyEnd:
		ui.sel = len(ui.rows) - 1
	case k == tui.Key{Kind: tui.KeyCtrl, Rune: 'u'} || k.Kind == tui.KeyPgUp:
		ui.move(-ui.pageSize() / 2)
	case k == tui.Key{Kind: tui.KeyCtrl, Rune: 'd'} || k.Kind == tui.KeyPgDn:
		// ctrl-d is half-page-down here, not cancel: vim muscle memory wins
		// inside a list this long. q and esc stay the exits.
		ui.move(ui.pageSize() / 2)
	case k.Kind == tui.KeyTab || k == tui.Key{Kind: tui.KeyRune, Rune: ' '}:
		ui.preview = !ui.preview
	case k == tui.Key{Kind: tui.KeyRune, Rune: '/'}:
		ui.mode = modeSearch
		ui.edit.SetString(ui.query)
	case k == tui.Key{Kind: tui.KeyRune, Rune: 'r'}:
		ui.startRename()
	case k == tui.Key{Kind: tui.KeyRune, Rune: 'd'}:
		if ui.selected() != nil {
			ui.mode = modePendingD
			ui.deadline = time.Now().Add(pendingDTimeout)
		}
	case k.Kind == tui.KeyEnter:
		return ui.commitResume(false)
	case k == tui.Key{Kind: tui.KeyRune, Rune: 'x'}:
		// Resume on the current account instead of the owner — and re-home:
		// from now on this session runs where the user just pointed it. Under
		// automatic placement, on the least-loaded of the other accounts.
		return ui.commitResume(true)
	}
	return false, 0
}

func (ui *resumeUI) selected() *sessions.Session {
	if ui.sel < 0 || ui.sel >= len(ui.rows) {
		return nil
	}
	return ui.rows[ui.sel]
}

func (ui *resumeUI) move(delta int) {
	if len(ui.rows) == 0 {
		return
	}
	ui.sel += delta
	if ui.sel < 0 {
		ui.sel = 0
	}
	if ui.sel >= len(ui.rows) {
		ui.sel = len(ui.rows) - 1
	}
}

// refilter rebuilds visible rows: local section first, then global, both
// newest-first; selection follows the session, not the index.
func (ui *resumeUI) refilter(query string) {
	var keepID string
	if s := ui.selected(); s != nil {
		keepID = s.ID
	}
	ui.query = query
	q := strings.ToLower(query)
	ui.rows = ui.rows[:0]
	for _, local := range []bool{true, false} {
		for _, s := range ui.listing.Sessions {
			if s.Local != local {
				continue
			}
			if q != "" && !matches(s, q) {
				continue
			}
			ui.rows = append(ui.rows, s)
		}
	}
	ui.sel = 0
	for i, s := range ui.rows {
		if s.ID == keepID {
			ui.sel = i
			break
		}
	}
}

func matches(s *sessions.Session, q string) bool {
	hay := strings.ToLower(strings.Join([]string{
		s.Tail.Title(), s.Head.Branch, s.Tail.ObservedBranch(), render.ProjectLabel(s), s.Owner, s.ID, s.Tail.Model,
	}, " "))
	return strings.Contains(hay, q)
}

// commitResume translates action outcomes into picker feedback or an exit.
func (ui *resumeUI) commitResume(override bool) (bool, int) {
	s := ui.selected()
	if s == nil {
		return false, 0
	}
	committed, err := ui.resume(s, override)
	if err != nil {
		if !committed {
			ui.message = err.Error()
			return false, 0
		}
		ui.failed = err
		return true, 1
	}
	return committed, 0
}

func (ui *resumeUI) startRename() {
	s := ui.selected()
	if s == nil {
		return
	}
	if err := ui.mutable(s, errOpenRename); err != nil {
		// The vendor's writer may hold this file open; interleaving with a
		// buffered append could land our record mid-line. A vendor mutation
		// stays bounded to files nobody else has open — refused before a
		// title is typed, not only at the commit.
		ui.message = err.Error()
		return
	}
	ui.mode = modeRename
	ui.edit.SetString(s.Tail.Title())
}

func (ui *resumeUI) commitRename() {
	s := ui.selected()
	title := strings.TrimSpace(render.Sanitize(ui.edit.String()))
	if s == nil || title == "" || title == s.Tail.Title() {
		return
	}
	if err := ui.rename(s, title); err != nil {
		ui.message = err.Error()
		return
	}

	s.Tail.CustomTitle = title
	ui.message = "renamed"
}

func (ui *resumeUI) commitDelete() {
	s := ui.selected()
	if s == nil {
		return
	}
	removed, err := ui.delete(s)
	if !removed {
		ui.message = err.Error()
		return
	}

	kept := ui.listing.Sessions[:0]
	for _, sess := range ui.listing.Sessions {
		if sess.ID != s.ID {
			kept = append(kept, sess)
		}
	}
	ui.listing.Sessions = kept
	ui.refilter(ui.query)
	ui.message = "deleted"
	if err != nil {
		ui.message += " — " + err.Error()
	}
}

// ---- rendering ----

// pageSize counts sessions per screen, not lines: every entry is two lines,
// and ctrl-d/u move in sessions.
func (ui *resumeUI) pageSize() int {
	_, h, err := ui.t.Size()
	if err != nil || h < 8 {
		h = 24
	}
	n := max((h-5)/2, 1) // header, section labels amortized, footer
	return n
}

// localSectionLabel names the local group the way the user thinks of it: the
// repo — worktree-aware, since not every checkout lives under the cwd — with
// the directory (~-abbreviated) as the non-repo fallback.
func (ui *resumeUI) localSectionLabel() string {
	for _, s := range ui.listing.Sessions {
		if s.Local && s.RepoKey != "" {
			return render.Sanitize(filepath.Base(s.RepoKey))
		}
	}
	if k := ui.listing.LocalKey; k != "" {
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(k, home+"/") {
			k = "~" + strings.TrimPrefix(k, home)
		}
		return render.Sanitize(k)
	}
	return "this repo"
}

func (ui *resumeUI) draw() {
	w, h, err := ui.t.Size()
	if err != nil || w <= 0 || h <= 0 {
		w, h = assumeWidth, assumeHeight
	}
	// Rows are laid out to at least a usable width, and every row is clipped
	// to the real one: a narrow pane reads less, never wraps into a second
	// physical row.
	layoutW := max(w, 20)
	now := time.Now().Unix()
	p := ui.p

	var lines []string
	title := ui.selectedTitleForHeader()
	lines = append(lines, p.Bold+"resume"+p.Rst+p.Dim+
		fmt.Sprintf(" · %d session(s) · new sessions → %s%s", len(ui.rows), render.ShortAccount(ui.shown()), title)+p.Rst)

	rowLines, selLine, selSpan := ui.rowLines(layoutW, now)
	body := max(h-3, 1) // header + footer + input/message line
	// Scroll to keep the selected entry's whole span — both lines plus the
	// expanded preview — in view; when the span outgrows the viewport, the
	// entry's first line wins.
	if selLine >= 0 {
		if bottom := selLine + selSpan; bottom > ui.top+body {
			ui.top = bottom - body
		}
		if selLine < ui.top {
			ui.top = selLine
		}
	}
	if ui.top > len(rowLines)-body {
		ui.top = len(rowLines) - body
	}
	if ui.top < 0 {
		ui.top = 0
	}
	end := min(ui.top+body, len(rowLines))
	lines = append(lines, rowLines[ui.top:end]...)
	for i := end - ui.top; i < body; i++ {
		lines = append(lines, "")
	}

	lines = append(lines, ui.inputLine())
	lines = append(lines, p.Dim+ui.hintLine()+p.Rst)

	if len(lines) > h {
		lines = lines[:h]
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	writeRows(&b, lines, w)
	fmt.Fprint(ui.t.Out(), b.String())
}

// selectedTitleForHeader states the second account pointer when it differs
// from the first: this repo's model is "independent facts stay independent",
// and a resumed session's account is not the new-session account.
func (ui *resumeUI) selectedTitleForHeader() string {
	s := ui.selected()
	if s == nil {
		return ""
	}
	target := s.Owner
	if target == "" {
		target = ui.shown()
	}
	if target == ui.shown() {
		return ""
	}
	return " · this session → " + render.ShortAccount(target)
}

// rowLines renders every visible row (plus section labels and the expanded
// preview) and reports where the selection starts and how many lines it
// spans — preview included, so the scroll can keep all of it in view.
func (ui *resumeUI) rowLines(w int, now int64) ([]string, int, int) {
	p := ui.p
	var lines []string
	selLine, selSpan := -1, 0
	section := -1
	for i, s := range ui.rows {
		sec := 0
		if !s.Local {
			sec = 1
		}
		if sec != section {
			section = sec
			label := ui.localSectionLabel()
			if sec == 1 {
				label = "elsewhere"
			}
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, p.Dim+label+p.Rst)
		}
		if i == ui.sel {
			selLine = len(lines)
		}
		lines = append(lines, ui.p.SessionLines(s, ui.shown(), i == ui.sel, w, now)...)
		if i == ui.sel {
			if ui.preview {
				lines = append(lines, ui.p.SessionPreview(s, w)...)
			}
			selSpan = len(lines) - selLine
		}
	}
	if len(ui.rows) == 0 {
		msg := "no sessions"
		if ui.query != "" {
			msg = "no sessions match " + render.Sanitize(ui.query)
		}
		lines = append(lines, p.Dim+msg+p.Rst)
	}
	return lines, selLine, selSpan
}

func (ui *resumeUI) inputLine() string {
	p := ui.p
	switch ui.mode {
	case modeSearch, modeRename:
		label := "/"
		if ui.mode == modeRename {
			label = "rename: "
		}
		// The rename field opens on the transcript's title, which another
		// program wrote: what is drawn is sanitized, and the buffer keeps the
		// text as read for the commit. A cursor on a rune that draws as nothing
		// still shows as a cell.
		before, at, after := ui.edit.Split()
		if at = render.Sanitize(at); at == "" {
			at = " "
		}
		return label + render.Sanitize(before) + p.Rev + at + p.Rst + render.Sanitize(after)
	case modeConfirmD:
		s := ui.selected()
		title := ""
		if s != nil {
			title = render.Sanitize(s.Tail.Title())
		}
		return p.Red + fmt.Sprintf("delete %q? y/n", title) + p.Rst
	default:
		if ui.message != "" {
			return ui.p.Yel + ui.message + ui.p.Rst
		}
		if ui.query != "" {
			return p.Dim + "filter: " + render.Sanitize(ui.query) + " (/ edits · esc in search clears)" + p.Rst
		}
		return ""
	}
}

func (ui *resumeUI) hintLine() string {
	switch ui.mode {
	case modeSearch:
		return "type to filter · enter keep · esc clear"
	case modeRename:
		return "enter save · esc cancel"
	case modeConfirmD:
		return "y delete · any other key cancels"
	default:
		// Kept under 80 cells: the pty harness proved anything longer clips
		// its own tail off on a standard terminal.
		return "enter resume · x re-home · spc preview · / find · r rename · dd delete · q quit"
	}
}
