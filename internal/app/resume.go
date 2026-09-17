package app

// The session picker owns selection, modes and terminal drawing. Collection,
// presentation and committed actions belong to sessions, render and
// sessionActions respectively.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/sessions"
	"github.com/qiushiyan/headroom/internal/state"
	"github.com/qiushiyan/headroom/internal/tui"
)

// execSessions is the exec edge, injected for tests. ExecPath, not Exec: the
// executable is resolved before the chdir, so a relative PATH entry in the
// project directory can never supply the binary.
var execSessions = launch.ExecPath

// psProbe keeps absent processes distinct from failed inspection.
func psProbe(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 && len(exit.Stderr) == 0 {
			return 0, os.ErrNotExist
		}
		return 0, err
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

func collectSessions(cfg config.Config, st *state.Store) (sessions.Listing, []sessions.AccountRef, []accounts.Account, string) {
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
		Owners:      st.Load().Owners(),
		Probe:       psProbe,
	})
	// Strict, exactly as launch resolves it: corrupt or dangling `.current`
	// yields "" — no current — rather than a primary nobody chose. Rows fall
	// back to the current account by doctrine, so a tolerant read here would
	// be the resume surface's route around the launch path's refusal.
	current := ""
	if sel, err := accounts.Select(cfg, accts, ""); err == nil {
		current = sel.Name
	}
	return listing, refs, accts, current
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

func runSessions(cfg config.Config, args []string) int {
	jsonMode := false
	cdFile := ""
	var claudeArgs []string
parse:
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonMode = true
		case "--cd-file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "headroom sessions: --cd-file needs a path")
				return 2
			}
			i++
			cdFile = args[i]
		case "--":
			claudeArgs = args[i+1:]
			break parse
		default:
			fmt.Fprintf(os.Stderr, "headroom sessions: unknown argument %q (claude args go after --)\n", args[i])
			return 2
		}
	}
	if jsonMode {
		if cdFile != "" || len(claudeArgs) > 0 {
			fmt.Fprintln(os.Stderr, "headroom sessions: --json takes no other arguments")
			return 2
		}
		return runSessionsJSON(cfg)
	}
	// This surface's contract is that the picker chooses the session; a
	// pass-through flag that chooses one contradicts it regardless of how
	// the vendor would rank the two.
	for _, a := range claudeArgs {
		if a == "--resume" || a == "-r" || a == "--continue" || a == "-c" {
			fmt.Fprintf(os.Stderr, "headroom sessions: %q cannot pass through — the picker chooses the session\n", a)
			return 2
		}
	}
	if cdFile != "" {
		// Absolute, same rule as every path here; created (or truncated) now,
		// before any side effect, so absence-of-content is observable even
		// through a wrapper that wrongly reuses a fixed path: empty means "no
		// launch was committed", non-empty means the dir was entered.
		if !filepath.IsAbs(cdFile) {
			fmt.Fprintf(os.Stderr, "headroom sessions: --cd-file %q is not absolute\n", cdFile)
			return 2
		}
		f, err := os.OpenFile(cdFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headroom sessions: --cd-file: %v\n", err)
			return 2
		}
		f.Close()
	}
	// Enter execs claude with the fds this process holds. A captured stdout
	// (`$(headroom sessions)`) would hand claude a pipe and hang the session
	// — the successor of the old line-protocol capture, refused up front.
	if !term.IsTerminal(int(os.Stdin.Fd())) || !stdoutIsTTY() {
		fmt.Fprintln(os.Stderr, "headroom sessions: execs claude in this terminal — run it without capturing stdin or stdout")
		return 1
	}

	st := state.Open(cfg.AccountsRoot)
	listing, refs, accts, current := collectSessions(cfg, st)
	t, err := tui.OpenTTY()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headroom sessions: %v\n", err)
		return 1
	}
	defer t.Close()

	ui := &resumeUI{t: t, p: render.NewPalette(true), listing: listing,
		sessionActions: sessionActions{cfg: cfg, st: st, refs: refs, accts: accts, current: current, cdFile: cdFile, claudeArgs: claudeArgs, beforeLaunch: t.Close}}
	if !st.Load().OwnersReadable() {
		// Routing has silently fallen back to derived evidence. Degraded
		// attribution is supposed to be visible, and this is the only moment
		// the user can be told that a re-home they made is not being applied.
		ui.message = "re-home records unreadable — routing by vendor evidence only (headroom check)"
	}
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
		fmt.Fprintf(os.Stderr, "headroom sessions: %v\n", err)
		return true, 1
	}
	return committed, 0
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
	if err != nil || w < 20 {
		w, h = 80, 24
	}
	now := time.Now().Unix()
	p := ui.p

	var lines []string
	title := ui.selectedTitleForHeader()
	lines = append(lines, p.Bold+"resume"+p.Rst+p.Dim+
		fmt.Sprintf(" · %d session(s) · new sessions → %s%s", len(ui.rows), render.ShortAccount(ui.current), title)+p.Rst)

	rowLines, selLine, selSpan := ui.rowLines(w, now)
	body := max(h-3, 3) // header + footer + input/message line
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
		lines = append(lines, ui.p.SessionLines(s, ui.current, i == ui.sel, w, now)...)
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
			msg = "no sessions match " + ui.query
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
