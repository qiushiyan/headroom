// Package state owns the one file headroom writes for itself: what it asked
// the usage endpoint, when, what came back, which sessions the user
// explicitly re-homed, which launches are on their way, and which homes spend
// against it.
//
// A file has two halves with different owners. The subscription ledger — the
// request ledger, the stored responses, the recent launches and the member
// homes — is about quota, keyed by account identity, and every home holding a
// login of a subscription must see one copy of it. The re-homes are about one
// home's sessions, named by that home's account names. A home alone keeps both
// in one file, as it always did; a home that shares another's ledger keeps its
// re-homes in its own file and spends against the other's.
//
// Three properties this package exists to guarantee, none of which a caller
// can be trusted to maintain:
//
//   - A request claim is a test-and-set. Eligibility is decided and the claim
//     written inside one locked section, so the permit Claim hands back — not
//     a flag computed earlier — is what authorizes traffic. Two processes
//     cannot both decide an account is eligible.
//   - Nothing this package does not understand is destroyed. Sections it
//     cannot decode are carried through writes verbatim, and a document from a
//     newer binary is never rewritten by an older one.
//   - The lock is never held across anything this package does not own: no
//     HTTP, no caller closures except the transcript enumerator that re-homing
//     genuinely needs inside the lock, and no re-entry (every method takes the
//     lock itself and no aggregate is exported, so there is no way to nest).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/placement"
	"github.com/qiushiyan/headroom/internal/sessions"
)

const (
	// Version is the schema this binary writes. A document carrying a higher
	// one is readable but never written: an older binary rewriting a newer
	// schema would silently truncate whatever it did not understand.
	Version = 1

	// CooldownBase is the quiet period after a refusal, doubling per
	// consecutive strike up to CooldownMax. Backoff means *no traffic*: a
	// refused request may itself count against the endpoint's budget, so
	// probing to discover recovery can prevent the recovery it probes for.
	CooldownBase = 2 * time.Minute
	CooldownMax  = 16 * time.Minute

	// BodyLimit caps a stored usage response. The fetch admits 4MB, and this
	// document is read and rewritten under a lock on every claim — an
	// oversized body would make every refresh pay for it. Observed payloads
	// are ~1-2KB.
	BodyLimit = 64 << 10

	// futureSkew is how far ahead of now a stored timestamp may sit before it
	// is treated as a clock anomaly rather than data. Wall clock is the only
	// axis available across processes; a step backwards must not leave an old
	// observation reading as current forever.
	futureSkew = 2 * time.Minute
)

// ErrReadOnly is returned by every mutation when the document on disk carries
// a schema this binary does not understand.
var ErrReadOnly = errors.New("state file was written by a newer headroom")

// ErrBusy is returned when the lock could not be acquired in time. Callers
// render it; they never block a redraw on it.
var ErrBusy = errors.New("state file is busy")

// ErrCorrupt is returned by mutations against a section that did not decode.
var ErrCorrupt = errors.New("state section is unreadable")

// ErrUnreadable is returned when the file is there but could not be read at
// all. It is deliberately not the same as an absent file: a document that
// exists holds re-homes, and writing a fresh one over bytes nobody could read
// would destroy them.
var ErrUnreadable = errors.New("state file could not be read")

// Key identifies an account for the request ledger.
//
// The endpoint's budget is per *account*, not per config dir — two dirs logged
// into the same account share one bucket — so the account's own UUID is the
// key whenever .claude.json reports one. Keying by dir name instead would let
// those two dirs double-spend a single bucket invisibly, and would need a
// separate guard against replaying a re-logged dir's old quota under its new
// login. Keying by identity deletes that guard rather than adding it: a
// re-logged dir simply has a different key, so there is nothing to replay.
type Key struct {
	UUID string // "" when .claude.json reports no accountUuid
	Name string // dir basename, or the primary's configured name
}

// ID is the map key in the document. The prefix keeps the two namespaces from
// colliding and makes the file readable with jq.
func (k Key) ID() string {
	if k.UUID != "" {
		return "uuid:" + k.UUID
	}
	return "dir:" + k.Name
}

// Observation is one stored usage response: the raw body exactly as the
// endpoint sent it, plus when it arrived.
//
// Raw, not parsed rows: usage.ParseLimits stays the only reader of that
// document, so the store cannot encode a row shape that drifts from the
// parser. Raw *bytes*, not a decoded-and-re-marshalled object: round-tripping
// through map[string]any would push numeric epoch timestamps through float64
// and silently mutate vendor data in the one component that promised not to
// interpret it.
//
// Held as a RawMessage, the body is re-emitted by the encoder as tokens, so
// insignificant whitespace is normalized while every value — number literals
// included, at full precision — survives exactly. That is the property that
// matters; byte-for-byte identity is not promised, and nesting the body as
// real JSON rather than an escaped string is what keeps the file readable
// with jq.
type Observation struct {
	FetchedAtMS int64           `json:"fetched_at_ms"`
	Body        json.RawMessage `json:"body"`
}

type request struct {
	LastAttemptMS  int64 `json:"last_attempt_ms"`
	NextEligibleMS int64 `json:"next_eligible_ms"`
	Strikes        int   `json:"strikes,omitempty"`
	Generation     int64 `json:"generation,omitempty"`
}

type accountRec struct {
	// Name is the dir this key was last seen as, carried for whoever reads the
	// file by hand. Nothing keys off it.
	Name    string       `json:"name,omitempty"`
	Request request      `json:"request"`
	Usage   *Observation `json:"usage,omitempty"`
}

// Problem is one thing wrong with the document itself — headroom's own file,
// never a statement about Claude Code. `check` reports these under their own
// heading so a corrupt state file can never read as vendor drift.
type Problem struct {
	Section string
	Detail  string
}

// doc is one decode of the file. Sections are kept both raw and typed: the
// raw map is what gets written back, so a section this binary could not decode
// survives a write by a section it could.
type doc struct {
	root     string // the accounts root this document was read from
	raw      map[string]json.RawMessage
	version  int
	accounts map[string]accountRec
	sessions map[string]sessions.OwnerRec

	// placements is the record of launches the placement rule reads: the
	// recent ones, which are load, and the newest per account. Disposable like
	// the request ledger — a section that will not decode is set aside and the
	// record starts empty, which costs a few launches their view of each other
	// and nothing else.
	placements    placement.Ledger
	badPlacements bool

	// members are the homes that spend against this ledger, keyed by accounts
	// root: where each keeps its account dirs, so another home can read their
	// live-session registries and count those sessions against the same
	// subscriptions. Disposable: a home re-registers at its next request or
	// launch.
	members    map[string]Member
	badMembers bool

	// A section that failed to decode is nil above and true here. The two
	// failures are handled differently on purpose: the ledger is disposable,
	// so it is quarantined and rebuilt conservatively, while re-homes are user
	// decisions, so they are never written over.
	badAccounts bool
	badSessions bool

	// dirty gates the write: a run where every account was inside its quiet
	// period changes nothing, and rewriting the file to say so would be pure
	// contention.
	dirty bool

	imported     *legacyImport
	migrationErr error

	// corruptDoc means the envelope itself would not decode, so the next write
	// moves the old bytes aside instead of overwriting them.
	corruptDoc bool

	// unreadable means the file exists and could not be read — a permission,
	// I/O or filesystem failure rather than anything about its contents. There
	// is then nothing to reason from and nothing this code may overwrite.
	unreadable bool

	problems []Problem
}

// legacyImport is a durable checkpoint and a bounded upgrade quiet period.
// One global deadline preserves every old name-keyed budget including identities now
// accessed through a different directory. Once expired it has no effect on request eligibility.
type legacyImport struct {
	QuietUntilMS int64 `json:"quiet_until_ms,omitempty"`
}

func (d *doc) readOnly() bool { return d.version > Version }

// Snapshot is a read-only view of the document. Reads take no lock: every
// write lands by atomic rename, so a reader sees one whole version or the
// previous one, never a torn file.
//
// Every failure short of "a newer schema" degrades to something usable: an
// absent file is an empty store, and a section that will not decode is
// reported rather than thrown away.
type Snapshot struct {
	d       *doc // the subscription ledger
	h       *doc // this home's own document; d itself when the ledger is the home's own
	vendor  config.Vendor
	spacing time.Duration
}

// Vendor is whose responses this snapshot holds: the store a body was read
// from says which vendor's parser reads it.
func (s Snapshot) Vendor() config.Vendor { return s.vendor }

func statePath(accountsRoot string) string {
	return filepath.Join(accountsRoot, "state.json")
}

// Member is one home that spends against a ledger. Root, its accounts root,
// is its identity; Home and Explicit are what another home needs to discover
// its account dirs (config.Scope's Home and PrimaryExplicit).
type Member struct {
	Root     string `json:"-"`
	Home     string `json:"home"`
	Explicit bool   `json:"explicit_primary,omitempty"`
	SeenAtMS int64  `json:"seen_at_ms"`
}

func read(accountsRoot string) *doc {
	d := &doc{
		root:     accountsRoot,
		raw:      map[string]json.RawMessage{},
		version:  Version,
		accounts: map[string]accountRec{},
		sessions: map[string]sessions.OwnerRec{},
		members:  map[string]Member{},
	}
	data, err := os.ReadFile(statePath(accountsRoot))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// The file is there and unreadable — permissions, an I/O error, a
			// filesystem that went away. Absent and unreadable must not be
			// conflated: the first means "nothing has happened yet" and the
			// second means "something is there and I cannot see it", and only
			// one of them makes it safe to write a fresh document.
			// Both sections are marked unavailable rather than empty: nothing
			// in there was read, so claiming the re-homes are readable would
			// be a second falsehood on top of the first.
			d.unreadable = true
			d.badAccounts, d.badSessions = true, true
			d.problems = append(d.problems, Problem{"document", err.Error() +
				" — nothing is being written until it can be read"})
			return d
		}
		// No document yet: whatever the legacy files hold is the starting
		// point, and the first write folds them in.
		d.importLegacy(accountsRoot)
		return d
	}
	if err := json.Unmarshal(data, &d.raw); err != nil {
		// The outer document is not JSON at all, so no section can be recovered
		// from it — per-section tolerance only works while the envelope holds.
		// The bytes are still moved aside rather than overwritten (commit does
		// it), the ledger stays conservative until a claim rebuilds it, and
		// `check` reports the whole thing.
		d.raw = map[string]json.RawMessage{}
		d.badAccounts = true
		d.corruptDoc = true
		d.problems = append(d.problems, Problem{"document",
			"not valid JSON — kept as state.json.unreadable; the ledger rebuilds conservatively"})
		return d
	}

	if v, ok := d.raw["version"]; ok {
		if err := json.Unmarshal(v, &d.version); err != nil {
			d.version = Version
			d.problems = append(d.problems, Problem{"version", "unreadable"})
		}
	} else {
		d.version = Version
	}

	if b, ok := d.raw["accounts"]; ok {
		if err := json.Unmarshal(b, &d.accounts); err != nil {
			d.accounts = map[string]accountRec{}
			d.badAccounts = true
			d.problems = append(d.problems, Problem{"accounts", "unreadable — quarantined; the request ledger rebuilds conservatively"})
		}
	}
	if b, ok := d.raw["sessions"]; ok {
		// Validated whole, exactly as the .owners reader this replaced did: a
		// section that decodes *around* a hollow record would have check report
		// it readable while routing silently ignores what it holds.
		if err := json.Unmarshal(b, &d.sessions); err != nil || !validOwners(d.sessions) {
			d.sessions = map[string]sessions.OwnerRec{}
			d.badSessions = true
			d.problems = append(d.problems, Problem{"sessions", "unreadable — session re-homes are unavailable and will not be overwritten"})
		}
	}
	if b, ok := d.raw["placements"]; ok {
		if err := json.Unmarshal(b, &d.placements); err != nil {
			d.placements = placement.Ledger{}
			d.badPlacements = true
			d.problems = append(d.problems, Problem{"placements", "unreadable — set aside at the next launch; placement starts from an empty record"})
		}
	}
	d.adoptHomeless()
	if b, ok := d.raw["members"]; ok {
		if err := json.Unmarshal(b, &d.members); err != nil {
			d.members = map[string]Member{}
			d.badMembers = true
			d.problems = append(d.problems, Problem{"members", "unreadable — set aside at the next request; every home registers again"})
		}
		for root, m := range d.members {
			// A record that cannot locate its home is no use to anyone, and
			// nothing but this code writes one.
			if !filepath.IsAbs(root) || !filepath.IsAbs(m.Home) {
				delete(d.members, root)
				continue
			}
			m.Root = root
			d.members[root] = m
		}
	}
	// A JSON null decodes into a map without error and leaves it nil. Nothing
	// here writes one, but a foreign or hand-edited document may, and a nil map
	// panics on the first assignment rather than failing any check above.
	if d.accounts == nil {
		d.accounts = map[string]accountRec{}
	}
	if d.sessions == nil {
		d.sessions = map[string]sessions.OwnerRec{}
	}
	if d.members == nil {
		d.members = map[string]Member{}
	}
	d.importLegacy(accountsRoot)
	return d
}

// adoptHomeless attributes launches recorded before homes shared a ledger to
// the home this file belongs to — the only home there was. Done on read, so a
// file a newer binary has not yet rewritten reads the same as one it has.
func (d *doc) adoptHomeless() {
	for i := range d.placements.Recent {
		if d.placements.Recent[i].Home == "" {
			d.placements.Recent[i].Home = d.root
		}
	}
	if d.placements.ByHome != nil || len(d.placements.Last) == 0 {
		return
	}
	var newest placement.Last
	for _, rec := range d.placements.Last {
		if rec.AtMS > newest.AtMS || (rec.AtMS == newest.AtMS && rec.Name < newest.Name) {
			newest = rec
		}
	}
	d.placements.ByHome = map[string]placement.Last{d.root: newest}
}

// register records a home in the ledger, and drops homes nobody has seen for
// the retention period. Only a change, or a record a day old, dirties the
// document: a claim that permits nothing must not become a write.
func (d *doc) register(m Member, now time.Time) {
	if !filepath.IsAbs(m.Root) || !filepath.IsAbs(m.Home) {
		// A home that cannot be located is no use to the others.
		return
	}
	if d.badMembers {
		d.quarantine("members")
		d.badMembers = false
		d.members = map[string]Member{}
		d.dirty = true
	}
	for root, cur := range d.members {
		if root != m.Root && cur.SeenAtMS < now.Add(-retention).UnixMilli() {
			delete(d.members, root)
			d.dirty = true
		}
	}
	cur, ok := d.members[m.Root]
	if ok && cur.Home == m.Home && cur.Explicit == m.Explicit && now.UnixMilli()-cur.SeenAtMS < memberRefresh.Milliseconds() {
		return
	}
	m.SeenAtMS = now.UnixMilli()
	d.members[m.Root] = m
	d.dirty = true
}

// Document is one of a snapshot's files as an audit sees it. A home that
// keeps its own ledger has one, holding everything; a home that shares
// another's has two — the ledger and its own re-homes — and each is judged on
// its own terms: a file written by a newer headroom is read and left alone
// without silencing what is wrong in the other.
type Document struct {
	// Name is "" for a home's one file, else "ledger" or "home".
	Name     string
	Version  int
	ReadOnly bool // written by a newer headroom: read, never written
	Problems []Problem

	Ledger  bool // holds the request ledger, the responses and the placements
	ReHomes bool // holds this home's re-homes
}

// Documents are the snapshot's files, the ledger first.
func (s Snapshot) Documents() []Document {
	if s.h == s.d {
		return []Document{{Version: s.d.version, ReadOnly: s.d.readOnly(), Problems: s.d.problems, Ledger: true, ReHomes: true}}
	}
	led := Document{Name: "ledger", Version: s.d.version, ReadOnly: s.d.readOnly(), Ledger: true}
	for _, p := range s.d.problems {
		led.Problems = append(led.Problems, Problem{"ledger " + p.Section, p.Detail})
	}
	home := Document{Name: "home", Version: s.h.version, ReadOnly: s.h.readOnly(), ReHomes: true}
	for _, p := range s.h.problems {
		switch p.Section {
		case "accounts", "placements", "members":
			// Left from before this home shared a ledger; nothing reads them.
		default:
			home.Problems = append(home.Problems, p)
		}
	}
	return []Document{led, home}
}

// Problems lists what could not be read, in every document. Empty is the
// ordinary case.
func (s Snapshot) Problems() []Problem {
	var out []Problem
	for _, d := range s.Documents() {
		out = append(out, d.Problems...)
	}
	return out
}

// Shared reports that the subscription ledger is another home's file.
func (s Snapshot) Shared() bool { return s.h != s.d }

// Members lists the homes registered in the ledger, by accounts root.
func (s Snapshot) Members() []Member {
	out := make([]Member, 0, len(s.d.members))
	for _, m := range s.d.members {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Member) int { return strings.Compare(a.Root, b.Root) })
	return out
}

// Observation returns the newest usage response headroom itself stored for
// this account, if any is usable.
//
// A body stamped in the future is not usable: wall clock is the only axis
// available across processes, and a clock step backwards would otherwise
// leave a two-day-old number rendering as "just now" until the clock caught
// up. Rejecting it costs one fetch; accepting it is a silent lie.
func (s Snapshot) Observation(k Key, now time.Time) (Observation, bool) {
	r, ok := s.d.accounts[k.ID()]
	if !ok || r.Usage == nil || len(r.Usage.Body) == 0 {
		return Observation{}, false
	}
	if r.Usage.FetchedAtMS <= 0 {
		return Observation{}, false
	}
	if r.Usage.FetchedAtMS > now.Add(futureSkew).UnixMilli() {
		return Observation{}, false
	}
	return *r.Usage, true
}

// NextEligible is when this account may next be asked; the zero time means
// "now". The board's scheduler uses it to pick the instant of the next round;
// what a *row* says comes from the claim's own answer, so that a rendered
// deadline is always the one the claim actually applied.
//
// A deadline further out than the maximum cooldown cannot have been written by
// this code and is clamped: a clock step forward while a cooldown was live
// would otherwise silence an account for as long as the step.
func (s Snapshot) NextEligible(k Key, now time.Time) time.Time {
	if s.d.badAccounts || s.d.readOnly() || s.d.migrationErr != nil {
		// Nothing here can be trusted to say an account is eligible, and
		// guessing "eligible" is the guess that generates traffic.
		return now.Add(ceiling(s.spacing))
	}
	r, ok := s.d.accounts[k.ID()]
	next := int64(0)
	if ok {
		next = r.Request.NextEligibleMS
	}
	if s.d.imported != nil && s.d.imported.QuietUntilMS > next {
		next = s.d.imported.QuietUntilMS
	}
	if next == 0 {
		return time.Time{}
	}
	if limit := now.Add(ceiling(s.spacing)); next > limit.UnixMilli() {
		return limit
	}
	return time.UnixMilli(next)
}

// Owner returns the explicit re-home recorded for a session, if any. Re-homes
// are this home's: another home sharing the ledger keeps its own.
func (s Snapshot) Owner(id string) (sessions.OwnerRec, bool) {
	r, ok := s.h.sessions[id]
	return r, ok
}

// Owners is every explicit re-home of this home. The map is the caller's to
// read, not to keep: a re-home is written by Place, with the launch it
// belongs to.
func (s Snapshot) Owners() map[string]sessions.OwnerRec {
	out := make(map[string]sessions.OwnerRec, len(s.h.sessions))
	maps.Copy(out, s.h.sessions)
	return out
}

// Placements is the record of launches as it stood when the document was read.
// It is the caller's to read, never to keep: a choice made on it is advice, and
// only Place — under the lock — may record one.
func (s Snapshot) Placements() placement.Ledger { return s.d.placements.Clone() }

// OwnersReadable reports whether the sessions section decoded. False means
// routing falls back to derived evidence, which the resume picker says out
// loud on open and `check` reports — rather than silently behaving as though
// no session was ever re-homed.
func (s Snapshot) OwnersReadable() bool { return !s.h.badSessions }

// AuditRecord is one ledger entry laid open for `check`. Nothing else reads
// the store this way: the surfaces ask about one account at a time and get
// answers, while the checker's job is to look at what is actually written.
type AuditRecord struct {
	ID             string
	Name           string
	FetchedAtMS    int64
	Body           json.RawMessage
	Strikes        int
	NextEligibleMS int64
}

// Audit lists every ledger entry in the document.
func (s Snapshot) Audit() []AuditRecord {
	out := make([]AuditRecord, 0, len(s.d.accounts))
	for id, r := range s.d.accounts {
		rec := AuditRecord{
			ID:             id,
			Name:           r.Name,
			Strikes:        r.Request.Strikes,
			NextEligibleMS: r.Request.NextEligibleMS,
		}
		if r.Usage != nil {
			rec.FetchedAtMS, rec.Body = r.Usage.FetchedAtMS, r.Usage.Body
		}
		out = append(out, rec)
	}
	return out
}

// validOwners reports that every re-home record is whole. headroom is the only
// writer of these, so a hollow one is corruption rather than an old shape — and
// one predicate serves both readers, because the section and the legacy file it
// absorbs hold the same records under the same rule.
func validOwners(m map[string]sessions.OwnerRec) bool {
	for id, rec := range m {
		if id == "" || rec.Account == "" || rec.AtMS <= 0 {
			return false
		}
	}
	return true
}

// mergeOwners folds src into dst newest-wins. Timestamps are event times from
// one machine's clock on one axis, so comparing them is meaningful — and
// newest-wins preserves decisions already in the current store during import.
func mergeOwners(dst, src map[string]sessions.OwnerRec) {
	for id, rec := range src {
		if cur, ok := dst[id]; !ok || rec.AtMS > cur.AtMS {
			dst[id] = rec
		}
	}
}

func (d *doc) marshal() ([]byte, error) {
	// Start from the raw sections so anything this binary did not decode —
	// a newer schema's addition, a quarantined section — survives untouched.
	out := make(map[string]json.RawMessage, len(d.raw)+3)
	maps.Copy(out, d.raw)
	v, err := json.Marshal(Version)
	if err != nil {
		return nil, err
	}
	out["version"] = v
	if d.imported != nil {
		out["legacy_import"], _ = json.Marshal(d.imported)
	}
	if !d.badAccounts {
		b, err := json.Marshal(d.accounts)
		if err != nil {
			return nil, err
		}
		out["accounts"] = b
	}
	if !d.badSessions {
		b, err := json.Marshal(d.sessions)
		if err != nil {
			return nil, err
		}
		out["sessions"] = b
	}
	if !d.badMembers {
		if len(d.members) == 0 {
			delete(out, "members")
		} else {
			b, err := json.Marshal(d.members)
			if err != nil {
				return nil, err
			}
			out["members"] = b
		}
	}
	if !d.badPlacements {
		if len(d.placements.Recent) == 0 && len(d.placements.Last) == 0 && len(d.placements.ByHome) == 0 {
			delete(out, "placements")
		} else {
			b, err := json.Marshal(d.placements)
			if err != nil {
				return nil, err
			}
			out["placements"] = b
		}
	}
	return json.MarshalIndent(out, "", "  ")
}

// quarantine moves a section's unreadable bytes aside under a name nothing
// reads, so the next write can rebuild the section without destroying what
// was there. Only the disposable ledger is ever quarantined.
func (d *doc) quarantine(section string) {
	if b, ok := d.raw[section]; ok {
		d.raw[fmt.Sprintf("%s_unreadable", section)] = b
		delete(d.raw, section)
	}
}
