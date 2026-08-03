package app

// resume is the session picker: every session on the machine in one list,
// project-local (the whole repo, worktrees included) above global, resumed
// on the account that last drove it. The TUI lives on /dev/tty so stdout
// carries exactly one machine-readable decision line for the shell wrapper —
// headroom decides and records, the shell cds and launches. This surface
// does zero network I/O and never touches the usage budget.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/tui"
)

// psProbe asks ps for a pid's start time — the one process inspection the
// liveness check needs. A dead pid errors, which Liveness reads as "the
// registry claim is stale", exactly right. lstart renders in the machine's
// local zone, so it is parsed as such and compared as an instant — the
// registry's own procStart string is UTC-rendered and must never be
// string-compared against this.
func psProbe(pid int) (int64, bool) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(out))
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", s, time.Local)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

func collectSessions(cfg config.Config) (sessions.Listing, []sessions.AccountRef, []accounts.Account, string) {
	accts := accounts.Discover(cfg)
	refs := make([]sessions.AccountRef, 0, len(accts))
	for _, a := range accts {
		refs = append(refs, sessions.AccountRef{Name: a.Name, Dir: a.Dir(cfg)})
	}
	cwd, _ := os.Getwd()
	listing := sessions.Collect(sessions.Input{
		ProjectsDir: cfg.ProjectsDir(),
		CWD:         cwd,
		Accounts:    refs,
		OwnersPath:  cfg.OwnersFile(),
		Probe:       psProbe,
	})
	return listing, refs, accts, accounts.CurrentTarget(cfg)
}

// resumeUI is the picker's whole mutable state. One mode value at a time —
// pending-delete, confirm, rename and search each replace it wholesale, so
// "renaming while a delete is pending" is unrepresentable.
type resumeUI struct {
	cfg     config.Config
	t       *tui.Terminal
	p       render.Palette
	listing sessions.Listing
	refs    []sessions.AccountRef
	accts   []accounts.Account
	current string // account name bare x targets; also the affinity fallback

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

func runResume(cfg config.Config, args []string) int {
	if len(args) == 1 && args[0] == "--json" {
		return runResumeJSON(cfg)
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "headroom resume: unexpected argument %q\n", args[0])
		return 2
	}

	listing, refs, accts, current := collectSessions(cfg)
	t, err := tui.OpenTTY()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom resume: %v\n", err)
		return 1
	}
	defer t.Close()

	ui := &resumeUI{cfg: cfg, t: t, p: render.NewPalette(true),
		listing: listing, refs: refs, accts: accts, current: current}
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
		case k := <-t.Events():
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
				if ui.liveNow(s) != sessions.NotLive {
					ui.message = "session is open elsewhere — not deleting"
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
		// from now on this session runs where the user just pointed it.
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
		s.Tail.Title(), s.Tail.Branch, projectLabel(s), s.Owner, s.ID,
	}, " "))
	return strings.Contains(hay, q)
}

// resumeAccount resolves which account a session resumes on: its owner when
// one is known and still exists, else the current account — visibly, via
// the row's owner tag, never silently.
func (ui *resumeUI) resumeAccount(s *sessions.Session, override bool) (accounts.Account, bool) {
	name := s.Owner
	if override || name == "" {
		name = ui.current
	}
	for _, a := range ui.accts {
		if a.Name == name {
			return a, true
		}
	}
	// The current account itself can be missing (state file names a removed
	// dir); the primary always exists.
	for _, a := range ui.accts {
		if a.IsPrimary() {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// liveNow re-establishes the selected session's liveness at action time —
// the listing's snapshot is display state, and a session opened since the
// picker drew must still gate every mutation and the resume refusal. The
// fresh answer also lands back on the row so the display tells the truth.
func (ui *resumeUI) liveNow(s *sessions.Session) sessions.LiveState {
	s.Live = sessions.LiveNow(s.ID, ui.refs, psProbe)
	return s.Live
}

// commitResume ends the session with the decision line on stdout. The frame
// goes down first (Close restores the alt screen), then exactly one write:
// project dir, session id, config dir — tab-separated, which is safe because
// rows whose paths embed a tab or newline were refused above.
func (ui *resumeUI) commitResume(override bool) (bool, int) {
	s := ui.selected()
	if s == nil {
		return false, 0
	}
	switch {
	case ui.liveNow(s) == sessions.Live:
		ui.message = "open in another terminal — switch there instead"
		return false, 0
	case !s.DirOK:
		ui.message = "project directory is gone — dd deletes the session"
		return false, 0
	case strings.ContainsAny(s.CWD, "\t\n\r"):
		ui.message = "project path contains control characters — not launchable"
		return false, 0
	}
	acct, ok := ui.resumeAccount(s, override)
	if !ok {
		ui.message = "no account to resume on"
		return false, 0
	}
	if strings.ContainsAny(acct.ConfigDir, "\t\n\r") {
		ui.message = "account dir contains control characters — not launchable"
		return false, 0
	}
	if override {
		// Recorded before the launch: source "the user pointed it here", to
		// be outranked by any newer vendor evidence on the same time axis.
		// `x`'s user-visible contract is atomic — re-home recorded, then the
		// launch decision — so a failed write keeps the picker open instead
		// of launching a session that would route back on the next enter.
		err := sessions.ReHome(ui.cfg.OwnersFile(), ui.cfg.ProjectsDir(), s.ID, acct.Name, time.Now())
		if err != nil {
			ui.message = "re-home not recorded (" + err.Error() + ") — enter resumes without it"
			return false, 0
		}
	}
	ui.t.Close()
	fmt.Printf("%s\t%s\t%s\n", s.CWD, s.ID, acct.ConfigDir)
	return true, 0
}

func (ui *resumeUI) startRename() {
	s := ui.selected()
	if s == nil {
		return
	}
	if ui.liveNow(s) != sessions.NotLive {
		// The vendor's writer may hold this file open; interleaving with a
		// buffered append could land our record mid-line. The one mutation
		// exception stays bounded to files nobody else has open.
		ui.message = "session is open elsewhere — rename there (Ctrl+R)"
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
	// Re-checked at commit, not only at mode entry: the user can sit in the
	// editor while another terminal opens this very session.
	if ui.liveNow(s) != sessions.NotLive {
		ui.message = "session is open elsewhere — rename there (Ctrl+R)"
		return
	}
	if err := sessions.AppendCustomTitle(s.Path, s.ID, title); err != nil {
		ui.message = "rename failed: " + err.Error()
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
	if ui.liveNow(s) != sessions.NotLive {
		ui.message = "session is open elsewhere — not deleting"
		return
	}
	if err := sessions.DeleteTranscript(s); err != nil {
		ui.message = "delete failed: " + err.Error()
		return
	}
	_ = sessions.ForgetOwner(ui.cfg.OwnersFile(), s.ID)
	kept := ui.listing.Sessions[:0]
	for _, sess := range ui.listing.Sessions {
		if sess.ID != s.ID {
			kept = append(kept, sess)
		}
	}
	ui.listing.Sessions = kept
	ui.refilter(ui.query)
	ui.message = "deleted"
}

// ---- rendering ----

func (ui *resumeUI) pageSize() int {
	_, h, err := ui.t.Size()
	if err != nil || h < 8 {
		h = 24
	}
	return h - 5 // header, section labels amortized, footer
}

func projectLabel(s *sessions.Session) string {
	switch {
	case s.RepoKey != "":
		return filepath.Base(s.RepoKey)
	case s.CWD != "":
		return filepath.Base(s.CWD)
	default:
		return filepath.Base(s.StoreDir)
	}
}

// localLabel distinguishes the checkouts of one repo: the branch in the main
// checkout, the worktree dir name elsewhere.
func localLabel(s *sessions.Session) string {
	if s.RepoRoot != "" && s.RepoRoot != s.RepoKey {
		return filepath.Base(s.RepoRoot)
	}
	return s.Tail.Branch
}

func ownerTag(s *sessions.Session, current string) string {
	switch s.OwnerState {
	case sessions.OwnerMissing:
		return "owner gone→" + shortAccount(current)
	case sessions.OwnerConflict:
		return "owner?→" + shortAccount(current)
	case sessions.OwnerNone:
		return "→" + shortAccount(current)
	default:
		return "@" + shortAccount(s.Owner)
	}
}

func shortAccount(name string) string {
	local, _, _ := strings.Cut(name, "@")
	return local
}

func (ui *resumeUI) draw() {
	w, h, err := ui.t.Size()
	if err != nil || w < 20 {
		w, h = 80, 24
	}
	now := time.Now().Unix()
	p := ui.p

	var lines []string
	title := ui.selectedTitleForHeader()
	lines = append(lines, p.Bold+"resume"+p.Rst+p.Dim+
		fmt.Sprintf(" · %d session(s) · new sessions → %s%s", len(ui.rows), shortAccount(ui.current), title)+p.Rst)

	rowLines, selLine := ui.rowLines(w, now)
	body := h - 3 // header + footer + input/message line
	if body < 3 {
		body = 3
	}
	// Scroll to keep the selected row in view.
	if selLine >= 0 {
		if selLine < ui.top {
			ui.top = selLine
		}
		if selLine >= ui.top+body {
			ui.top = selLine - body + 1
		}
	}
	if ui.top > len(rowLines)-body {
		ui.top = len(rowLines) - body
	}
	if ui.top < 0 {
		ui.top = 0
	}
	end := ui.top + body
	if end > len(rowLines) {
		end = len(rowLines)
	}
	lines = append(lines, rowLines[ui.top:end]...)
	for i := end - ui.top; i < body; i++ {
		lines = append(lines, "")
	}

	lines = append(lines, ui.inputLine())
	lines = append(lines, p.Dim+ui.hintLine()+p.Rst)

	var b strings.Builder
	b.WriteString("\x1b[H")
	for _, s := range lines {
		b.WriteString("\x1b[2K" + render.Clip(s, w) + "\r\n")
	}
	b.WriteString("\x1b[J")
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
		target = ui.current
	}
	if target == ui.current {
		return ""
	}
	return " · this session → " + shortAccount(target)
}

// rowLines renders every visible row (plus section labels and the expanded
// preview) and reports which line the selection sits on.
func (ui *resumeUI) rowLines(w int, now int64) ([]string, int) {
	p := ui.p
	var lines []string
	selLine := -1
	section := -1
	for i, s := range ui.rows {
		sec := 0
		if !s.Local {
			sec = 1
		}
		if sec != section {
			section = sec
			label := "this repo"
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
		lines = append(lines, ui.rowLine(s, i == ui.sel, w, now))
		if i == ui.sel && ui.preview {
			lines = append(lines, ui.previewLines(s, w)...)
		}
	}
	if len(ui.rows) == 0 {
		msg := "no sessions"
		if ui.query != "" {
			msg = "no sessions match " + ui.query
		}
		lines = append(lines, p.Dim+msg+p.Rst)
	}
	return lines, selLine
}

func (ui *resumeUI) rowLine(s *sessions.Session, selected bool, w int, now int64) string {
	p := ui.p
	title := render.Sanitize(s.Tail.Title())
	if title == "" {
		title = "⟨untitled⟩"
	}

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
	label := localLabel(s)
	if !s.Local {
		label = projectLabel(s)
	}
	meta := []string{}
	if label != "" {
		meta = append(meta, render.Sanitize(label))
	}
	meta = append(meta, ownerTag(s, ui.current), render.Age(now-s.MTime.Unix()))
	metaStr := strings.Join(meta, " · ")
	if len(marks) > 0 {
		metaStr = strings.Join(marks, " ") + " " + metaStr
	}

	prefix := "  "
	if selected {
		prefix = p.Bold + "▶ " + p.Rst
	}
	// Fixed right column, flexing title. Cell arithmetic, not runes: titles
	// are the first place a CJK string meets framePrinter-style layouts.
	metaCells := render.Cells(stripSGR(metaStr))
	titleWidth := w - 2 - metaCells - 2
	if titleWidth < 8 {
		titleWidth = 8
	}
	line := prefix + render.PadCell(title, titleWidth) + "  " + p.Dim + metaStr + p.Rst
	if selected {
		return line
	}
	return line
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

const previewCap = 3 // lines per part — context, not a pager

func (ui *resumeUI) previewLines(s *sessions.Session, w int) []string {
	p := ui.p
	var out []string
	add := func(prefix, text string) {
		if text == "" {
			return
		}
		text = render.Sanitize(text)
		width := w - 8
		if width < 10 {
			width = 10
		}
		for i, line := 0, ""; text != "" && i < previewCap; i++ {
			line = render.TrimCells(text, width)
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

func (ui *resumeUI) inputLine() string {
	p := ui.p
	switch ui.mode {
	case modeSearch, modeRename:
		label := "/"
		if ui.mode == modeRename {
			label = "rename: "
		}
		before, at, after := ui.edit.Split()
		return label + before + p.Rev + at + p.Rst + after
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
			return p.Dim + "filter: " + ui.query + " (/ edits · esc in search clears)" + p.Rst
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
