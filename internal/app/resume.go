package app

// sessions is the session picker: every session on the machine in one list,
// project-local (the whole repo, worktrees included) above global, continued
// on the account that last drove it. The picker itself enters the verified
// project dir and execs claude through the same launch.Target seam `launch`
// uses — no routing decision ever leaves this process. The one thing only
// the parent shell can do, make its own cd stick after the session ends, is
// served by an advisory --cd-file: headroom writes the entered dir there
// before exec, and nothing about routing depends on whether anyone reads it.
// This surface does zero network I/O and never touches the usage budget.
//
// Its predecessor printed a dir/id/account decision line for the wrapper to
// recombine; a stale shell function misread the account field as a relative
// config dir and claude ran the session as the primary (see DESIGN.md § The
// launch surface). The verb changed with the contract — resume's tombstone
// in Run is the only observable remnant of the line protocol.

import (
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
		Owners:      ownerRecords(st.Load()),
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

// ownerRecords adapts the store's re-home records to the collector's own type.
// The two are deliberately separate: sessions reads vendor files and nothing
// else, and giving it a dependency on the package that flocks and writes would
// put I/O in the graph of the one place that is meant to be pure parsing.
func ownerRecords(snap state.Snapshot) map[string]sessions.OwnerRec {
	src := snap.Owners()
	out := make(map[string]sessions.OwnerRec, len(src))
	for id, rec := range src {
		out[id] = sessions.OwnerRec{Account: rec.Account, AtMS: rec.AtMS}
	}
	return out
}

// resumeUI is the picker's whole mutable state. One mode value at a time —
// pending-delete, confirm, rename and search each replace it wholesale, so
// "renaming while a delete is pending" is unrepresentable.
type resumeUI struct {
	cfg        config.Config
	st         *state.Store
	t          *tui.Terminal
	p          render.Palette
	listing    sessions.Listing
	refs       []sessions.AccountRef
	accts      []accounts.Account
	current    string   // account name bare x targets; also the affinity fallback
	cdFile     string   // advisory: the entered dir is written here before exec ("" = none)
	claudeArgs []string // personal flags, verbatim after --; the session flag is this surface's own

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

	ui := &resumeUI{cfg: cfg, st: st, t: t, p: render.NewPalette(true),
		listing: listing, refs: refs, accts: accts, current: current,
		cdFile: cdFile, claudeArgs: claudeArgs}
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
		s.Tail.Title(), s.Tail.Branch, projectLabel(s), s.Owner, s.ID, s.Tail.Model,
	}, " "))
	return strings.Contains(hay, q)
}

// resumeAccount resolves which account a session resumes on: its owner when
// one is known and still exists, else the current account — visibly, via
// the row's owner tag, never silently. There is no further rung: ui.current
// is "" when `.current` failed strict resolution, and a session neither the
// owner evidence nor a valid current can place is refused rather than routed
// to the primary — a fallback the launch path just stopped minting from
// corrupt state must not reappear here as an attribution courtesy.
func (ui *resumeUI) resumeAccount(s *sessions.Session, override bool) (accounts.Account, bool) {
	name := s.Owner
	if override || name == "" {
		name = ui.current
	}
	if name == "" {
		return accounts.Account{}, false
	}
	for _, a := range ui.accts {
		if a.Name == name {
			return a, true
		}
	}
	// The owner names an account the filesystem no longer has: degraded
	// attribution falls back to the *current* account — the row's owner tag
	// already says so — never to the primary, which no evidence chose.
	if name != ui.current && ui.current != "" {
		for _, a := range ui.accts {
			if a.Name == ui.current {
				return a, true
			}
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

// commitResume ends the picker by becoming the session: every refusal is
// checked first — returning to the picker with a message, so the user picks
// another row instead of being dumped onto a torn-down frame — then the
// terminal is restored, the process enters the verified project dir, the
// advisory cd file gets the dir, and exec replaces this image with claude on
// the resolved account's environment. Ordering is load-bearing: refusals
// (the re-home write included, which must not record a pointing that then
// refused to launch) precede Close; the executable is resolved before the
// chdir; the cd file is written only after the chdir succeeds, so non-empty
// means "this dir was entered".
//
// Deliberately absent: --remember. `.current` means "where new sessions go"
// and only the account board moves it — the exec makes adding it tempting,
// and it must stay refused (DESIGN.md § The session surface).
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
	case strings.Contains(s.CWD, "\n"):
		// The one representational limit left now that no line protocol
		// exists: the cd file is line-shaped. Tabs and the old account-name
		// check went with the protocol that needed them.
		ui.message = "project path contains a newline — not representable in the cd file"
		return false, 0
	}
	// Action-time re-stat: the listing's DirOK is display state, and worktree
	// churn deletes project dirs constantly.
	if fi, err := os.Stat(s.CWD); err != nil || !fi.IsDir() {
		ui.message = "project directory is gone — dd deletes the session"
		return false, 0
	}
	acct, ok := ui.resumeAccount(s, override)
	if !ok {
		ui.message = "no account to resume on — run headroom accounts"
		return false, 0
	}
	tgt, err := target(acct)
	if err != nil {
		ui.message = err.Error()
		return false, 0
	}
	if acct.IsPrimary() && ui.cfg.PrimaryRelocated {
		ui.message = "HEADROOM_HOME re-points the primary — cannot continue on it here"
		return false, 0
	}
	if err := accounts.VerifyTopology(ui.cfg, acct); err != nil {
		ui.message = "not launching — " + err.Error()
		return false, 0
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		ui.message = "claude not found on PATH"
		return false, 0
	}
	if override {
		// Recorded before the launch: source "the user pointed it here", to
		// be outranked by any newer vendor evidence on the same time axis.
		// `x`'s user-visible contract is atomic — re-home recorded, then the
		// launch decision — so a failed write keeps the picker open instead
		// of launching a session that would route back on the next enter.
		// The sweep of records whose transcript is gone runs inside the store's
		// lock, against a listing taken there — never against one gathered
		// here, which could not see a session another picker re-homed since.
		projects := ui.cfg.ProjectsDir()
		err := ui.st.ReHome(s.ID, acct.Name, time.Now(), func() (map[string]bool, bool) {
			return sessions.TranscriptIDs(projects)
		})
		if err != nil {
			ui.message = "re-home not recorded (" + err.Error() + ") — enter resumes without it"
			return false, 0
		}
	}
	if ui.t != nil { // nil only under go test; the pty harness drives the real terminal
		ui.t.Close()
	}
	if err := os.Chdir(s.CWD); err != nil {
		// Past the re-stat is a TOCTOU loss; the frame is already down, so
		// this is a hard exit, not a picker message.
		fmt.Fprintf(os.Stderr, "headroom sessions: cd %s: %v\n", s.CWD, err)
		return true, 1
	}
	if ui.cdFile != "" {
		if err := writeCDFile(ui.cdFile, s.CWD); err != nil {
			// One stderr line, never a refusal: the sticky cd is convenience,
			// and the decision the user just made is not thrown away for it.
			fmt.Fprintf(os.Stderr, "headroom sessions: cd file: %v (continuing)\n", err)
		}
	}
	argv := append(append([]string{}, ui.claudeArgs...), "--resume", s.ID)
	if err := execSessions(claudePath, argv, envWithPWD(tgt.Env(os.Environ()), s.CWD)); err != nil {
		fmt.Fprintf(os.Stderr, "headroom sessions: exec claude: %v\n", err)
		return true, 1
	}
	return true, 0
}

// envWithPWD replaces the inherited PWD with the entered dir: os.Chdir moves
// the kernel cwd but not the environment, and the shell-set PWD would
// otherwise describe the directory the picker was invoked from.
func envWithPWD(env []string, cwd string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PWD=") {
			out = append(out, kv)
		}
	}
	return append(out, "PWD="+cwd)
}

// writeCDFile publishes the entered dir to the advisory file created at flag
// parse. Truncate-then-write on the already-created path; a Sync so the line
// survives the process being replaced moments later.
func writeCDFile(path, cwd string) error {
	f, err := os.OpenFile(path, os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(cwd); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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
	_ = ui.st.Forget(s.ID)
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

// pageSize counts sessions per screen, not lines: every entry is two lines,
// and ctrl-d/u move in sessions.
func (ui *resumeUI) pageSize() int {
	_, h, err := ui.t.Size()
	if err != nil || h < 8 {
		h = 24
	}
	n := (h - 5) / 2 // header, section labels amortized, footer
	if n < 1 {
		n = 1
	}
	return n
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
	if name == "" {
		// No valid current account: `.current` failed strict resolution.
		// "?" over a name nobody chose.
		return "?"
	}
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

	rowLines, selLine, selSpan := ui.rowLines(w, now)
	body := h - 3 // header + footer + input/message line
	if body < 3 {
		body = 3
	}
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
		lines = append(lines, ui.sessionLines(s, i == ui.sel, w, now)...)
		if i == ui.sel {
			if ui.preview {
				lines = append(lines, ui.previewLines(s, w)...)
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

// sessionLines renders one session as a two-line entry. The first line
// carries what muscle memory recognizes a session by — where it ran (branch
// or worktree locally, branch·project elsewhere), whether it is open right
// now, and the meta column: model, account, age. The second line, dimmed,
// is just the confirming title.
func (ui *resumeUI) sessionLines(s *sessions.Session, selected bool, w int, now int64) []string {
	p := ui.p

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
		marksCells = render.Cells(stripSGR(marksStr)) + 2
	}

	// The meta column budgets itself before the label flexes, and sheds from
	// its own left when the terminal is narrow — the model first, the account
	// second, the age never: the final Clip cuts lines from the right, and a
	// row without its timestamp is the one degradation this repo forbids.
	var meta []string
	if m := modelLabel(s.Tail.Model); m != "" {
		meta = append(meta, m)
	}
	meta = append(meta, ownerTag(s, ui.current), render.Age(now-s.MTime.Unix()))
	metaAvail := w - 2 - 4 - 2 - marksCells // prefix, minimum label, gap
	if metaAvail < 2 {
		metaAvail = 2
	}
	for len(meta) > 1 && render.Cells(strings.Join(meta, " · ")) > metaAvail {
		meta = meta[1:]
	}
	right := strings.Join(meta, " · ")
	if render.Cells(right) > metaAvail {
		right = render.TrimCells(right, metaAvail)
	}

	prefix := "  "
	if selected {
		prefix = p.Bold + "▶ " + p.Rst
	}
	// Flexing label over a fixed right column. Cell arithmetic, not runes:
	// vendor text is the first place a CJK string meets framePrinter-style
	// layouts.
	label := render.Sanitize(primaryLabel(s))
	labelWidth := w - 2 - render.Cells(right) - 2 - marksCells
	if labelWidth < 4 {
		labelWidth = 4
	}
	if render.Cells(label) > labelWidth {
		label = render.TrimCells(label, labelWidth-1) + "…"
	}
	left := prefix + label
	if marksStr != "" {
		left += "  " + marksStr
	}
	pad := w - render.Cells(stripSGR(left)) - render.Cells(right)
	if pad < 2 {
		pad = 2
	}
	line1 := left + strings.Repeat(" ", pad) + p.Dim + right + p.Rst

	title := render.Sanitize(s.Tail.Title())
	if title == "" {
		title = "⟨untitled⟩"
	}
	titleWidth := w - 4
	if titleWidth < 8 {
		titleWidth = 8
	}
	if render.Cells(title) > titleWidth {
		title = render.TrimCells(title, titleWidth-1) + "…"
	}
	line2 := "    " + p.Dim + title + p.Rst
	return []string{line1, line2}
}

// primaryLabel is a session's first-line identity — where it ran: the
// checkout (branch in the main checkout, worktree dir name elsewhere), with
// the project appended as the disambiguator in the global section. Both
// sections resolve the checkout the same way — a worktree session must not
// change identity by scrolling past the section break.
func primaryLabel(s *sessions.Session) string {
	if s.Local {
		if l := localLabel(s); l != "" {
			return l
		}
		return projectLabel(s)
	}
	proj := projectLabel(s)
	if l := localLabel(s); l != "" && l != proj {
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
