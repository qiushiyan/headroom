package app

// Refresh cadence is eligibility itself: there is no interval to pick, because
// asking sooner is refused by the endpoint's budget and asking later would
// leave the board saying less than it could. What replaces watch's "the user
// chose to run a long-lived command" is presence — a picker left open on a
// spare tab must not poll for hours with nobody looking, which would be the
// background daemon this design deliberately does not have, wearing a TUI.

import (
	"context"
	"time"

	"github.com/qiushiyan/headroom/internal/accountstate"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/round"
	"github.com/qiushiyan/headroom/internal/state"
)

// presenceWindow is how long after the last keypress the board keeps
// refreshing itself. Past it the loop goes quiet until a key lands.
const presenceWindow = 5 * time.Minute

// page is one vendor's page of the picker: its newest round's board, its
// scroll position and its refresh schedule. A page owns its
// in-flight round — one list and one channel until that channel closes and
// drains — so a result can only ever land on the page that started the round,
// never on whichever page happens to be visible when it arrives.
//
// The schedule is the page's own too, and it moves by four events and nothing
// else: a round started for it (startRound, when it is first shown), the `r`
// key (refresh), the loop's second (tick) and a result delivered or its
// channel closing (receive). What the schedule then is reads back through
// refreshStatus, the footer's words. Whether a round is in flight is the
// channel itself, never a second flag beside it; when the page last ran its
// local half and when it is next due are instants on the page's one clock.
type page struct {
	scope config.Scope
	st    *state.Store
	deps  BoardDeps
	// round is the newest round's: the accounts on screen and the set they
	// were discovered as, under the page's scope and store, which every round
	// keeps. nil until the first round.
	round *round.Round

	updates <-chan round.Result // non-nil while this page's round is in flight

	lastLocal time.Time // when the local half last ran — the floor's clock
	nextAt    time.Time // next scheduled cadence round
	armed     bool      // r pressed while floored or mid-round; fires when the floor allows
	manual    bool      // current round was asked for by hand: acknowledge on completion
	ack       string    // transient acknowledgement after a manual round
	ackUntil  time.Time
	top       int // first body line in view when the board outgrows the terminal

	// placed is what the last round gathered for the marks, from which every
	// frame marks again at its own time; nil until a round has marked.
	placed *launchop.Marks

	// begin starts a round under the page's scope and store: the local half,
	// then the claim and whatever fetches it permits. The round it returns
	// replaces the page's. nil means beginRound; tests inject channels.
	begin func(ctx context.Context, pg *page) (*round.Round, <-chan round.Result)

	// now is the page's clock: every instant the schedule records or
	// compares is read from it. nil is the wall clock; tests hold one.
	now func() time.Time
}

// clock reads the page's clock.
func (pg *page) clock() time.Time {
	if pg.now != nil {
		return pg.now()
	}
	return time.Now()
}

const ackTTL = 4 * time.Second

func newPage(scope config.Scope, deps BoardDeps) *page {
	return &page{scope: scope, st: state.Open(scope), deps: deps}
}

// accounts is the newest round's accounts, as they stand; none before the
// first round.
func (pg *page) accounts() []accountstate.Account {
	if pg.round == nil {
		return nil
	}
	return pg.round.Accounts()
}

// receive applies one result of this page's own round, or closes the round
// out when its channel has drained. Only this receiving path updates facts.
func (pg *page) receive(u round.Result, open bool) {
	if open {
		pg.round.Apply(u)
		return
	}
	now := pg.clock()
	pg.updates = nil
	pg.mark(now)
	pg.schedule()
	if pg.manual {
		// The user asked; the answer is what changed, said once and briefly —
		// a claim persisting past its moment is how the old "refresh armed"
		// read as being ignored.
		pg.ack, pg.ackUntil = pg.ackString(now), now.Add(ackTTL)
		pg.manual = false
	}
}

// startRound runs one full round: a local half — re-read everything free
// (health, discovery, credentials, the store's replay) — and a
// budget half, the claim plus whatever fetches it permits. The two halves are
// named because only the first needs the floor: it costs a `claude auth
// status` spawn per account, which a held key must not spin, while the second
// is priced by the claim itself. They always run together — a round that
// skipped the claim would freeze the schedule (nothing would recompute
// nextAt), and one that skipped the local half would fetch against a stale
// registry. The store is re-read every round rather than held: the picker can
// sit open for hours, and a snapshot taken at startup cannot see claims
// another surface made since.
func (ui *page) startRound(ctx context.Context, manual bool) {
	begin := ui.begin
	if begin == nil {
		begin = beginRound
	}
	r, updates := begin(ctx, ui)
	ui.round = r
	now := ui.clock()
	ui.mark(now)
	ui.lastLocal = now
	ui.armed = false
	ui.manual = manual
	ui.updates = updates
}

// beginRound is a round as the picker runs it, under the picker's ctx: the
// local half with the health probe, then the claim and whatever fetches it
// permits, against the page's own store.
func beginRound(ctx context.Context, pg *page) (*round.Round, <-chan round.Result) {
	r := round.Prepare(ctx, pg.scope, pg.st, pg.clock(), pg.deps.Round)
	return r, r.Fetch(ctx)
}

// mark gathers what the marks are decided from and sets every row's load and
// the row a launch would take from the figures this page holds. It runs when
// a round starts — the stored figures are already worth a mark — and again
// when its results have landed.
func (ui *page) mark(now time.Time) {
	ui.placed = markBoard(ui.round, now, ui.deps.Place)
}

// markAt marks the rows again from what the last round gathered, at now. A
// board left open stops refreshing once nobody is at the keys, and the clock
// alone can change the choice; the frame that shows a mark decides it.
func (ui *page) markAt(now time.Time) {
	if ui.placed != nil {
		ui.round.Mark(ui.placed.At(now))
	}
}

// ackString is the one-line answer to a manual refresh, composed after the
// round lands: either everything obtainable was obtained, or the budget is
// holding some account back and the soonest retry is named. Per-row detail
// (which account, why) is already on the rows via their attempt captions.
func (ui *page) ackString(now time.Time) string {
	var next time.Time
	for _, d := range ui.accounts() {
		if d.View.Attempt.State.Answered() {
			continue
		}
		if at := d.View.Attempt.NextEligibleAt; at > now.Unix() {
			t := time.Unix(at, 0)
			if next.IsZero() || t.Before(next) {
				next = t
			}
		}
	}
	if next.IsZero() {
		return "refreshed · all current"
	}
	return "refreshed · usage in " + until(next, now)
}

// schedule sets the next round for the moment the first account becomes
// eligible again — the soonest instant at which asking could return anything
// new, and therefore the only cadence worth having. There is no interval to
// configure: sooner is refused by the endpoint's budget, later would leave the
// board saying less than it could.
func (ui *page) schedule() {
	ui.nextAt = nextRound(ui.st.Load(), ui.accounts(), ui.round != nil && ui.round.Asking(), ui.scope.RequestSpacing(), ui.clock())
}

// nextRound is the instant the next round is due, from the store as it now
// stands: the soonest any account becomes eligible again, never sooner than
// the floor. asking says the round had anything to ask about.
func nextRound(snap state.Snapshot, accts []accountstate.Account, asking bool, spacing time.Duration, now time.Time) time.Time {
	var next time.Time
	for _, d := range accts {
		if e := snap.NextEligible(d.Key, now); e.After(now) && (next.IsZero() || e.Before(next)) {
			next = e
		}
	}
	// The floor is a floor, never a ceiling: a round whose claims all failed
	// records no eligibility at all, and without it the loop would retry as
	// fast as the ticker runs.
	if floor := now.Add(refreshFloor); next.IsZero() || next.Before(floor) {
		next = floor
	}
	// Unless nothing was asking in the first place. With every account logged
	// out or its token aged out there is no eligibility to wait for, and the
	// floor would become the cadence — spawning `claude auth status` per
	// account twice a minute to re-learn the same answer. The request spacing
	// is the right rate for that too: it is the slowest cadence at which this
	// board ever tells the user something new, and it must stay *under* the
	// presence window, which is measured from the same instant and would
	// otherwise have expired by the time the round came due — a poll scheduled
	// past its own gate never fires at all.
	if !asking {
		next = now.Add(spacing)
	}
	return next
}

// refreshFloor is the minimum spacing between rounds, and it exists for the
// local half's spawn cost alone — never as budget arithmetic, which is the
// claim's. No scheduled cadence can hit it (every scheduled round is at least
// a request spacing away); only a hand-delivered `r` can, so treating it as
// an interval to tune would turn spawn hygiene into a polling rate.
const refreshFloor = 30 * time.Second

// due reports whether a round should start now. Presence is part of the
// question: an unattended board stops asking, and says so. An armed round —
// one the user asked for — fires the moment the floor allows, attended or
// not: it was asked for.
func (ui *page) due(lastKey time.Time) bool {
	if ui.updates != nil {
		return false
	}
	now := ui.clock()
	if ui.armed {
		return !now.Before(ui.lastLocal.Add(refreshFloor))
	}
	if ui.nextAt.IsZero() || now.Before(ui.nextAt) {
		return false
	}
	return now.Sub(lastKey) < presenceWindow
}

// tick is the loop's second on the visible page: a round that is due starts.
// lastKey is when a key last landed on the board — presence. A round due
// because it was armed is still the user's ask.
func (ui *page) tick(ctx context.Context, lastKey time.Time) {
	if ui.due(lastKey) {
		ui.startRound(ctx, ui.armed)
	}
}

// refreshStatus is how the schedule reads, for the footer: a round in flight
// (and whether another is queued behind it), one queued behind the floor, the
// answer to a round asked for by hand, a board nobody is at, or when the next
// round is due. "" before the first round has completed. What it says and
// what due decides are read from the same fields at the same clock; an
// acknowledgement on show does not hold a due round back.
func (ui *page) refreshStatus(lastKey time.Time) string {
	now := ui.clock()
	switch {
	case ui.updates != nil && ui.armed:
		return "refreshing… · queued"
	case ui.updates != nil:
		return "refreshing…"
	case ui.armed:
		return "queued · refresh in " + until(ui.lastLocal.Add(refreshFloor), now)
	case ui.ack != "" && now.Before(ui.ackUntil):
		return ui.ack
	case ui.nextAt.IsZero():
		return "" // no round has completed yet
	case now.Sub(lastKey) >= presenceWindow:
		return "paused — r to refresh"
	}
	return "next refresh in " + until(ui.nextAt, now)
}

// refresh is the `r` key: ask now. The claim — never this loop — decides per
// account whether a request may leave, so the round runs immediately and a
// still-cooling account comes back annotated with when it can next be asked,
// rather than the key silently re-arming the cadence. The two states that
// defer it both remember the press (`r` is never a no-op): a round already in
// flight only carries the accounts eligible at its start, and a press inside
// the floor is deferred spawn hygiene, not budget politeness — the queued
// round fires the moment the floor passes, unattended included.
func (ui *page) refresh(ctx context.Context) {
	ui.ack, ui.ackUntil = "", time.Time{} // a new ask supersedes the old answer
	if ui.updates != nil || ui.clock().Before(ui.lastLocal.Add(refreshFloor)) {
		ui.armed = true
		return
	}
	ui.startRound(ctx, true)
}

func until(t, now time.Time) string {
	d := max(t.Sub(now), 0)
	return d.Round(time.Second).String()
}
