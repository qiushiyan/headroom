# Readings from other machines — one home's figures, current wherever the account is used

Status: built on the `peer-readings` branch, 2026-11-27, and revised by its
review (rounds 1 and 2, Astra: § Review revisions); kept as the dated record
of the decision. The design as it stands lives in `DESIGN.md` § Readings from
other machines — read that, not this, for what is true today. Written against
`main` at a0feb6b, on two macOS machines (a laptop and a desk mini) holding
the same seven Claude Code accounts.

## Summary

Current: each machine's headroom asks the usage endpoint only about the
accounts whose access token it holds live, and only a Claude Code session on
that machine keeps a token live. An account used on one machine and idle on
another reads current on the first and frozen on the second: measured
2026-11-27, cliushi@ read 80% / 88% Fable on the mini (2 minutes old) and 57% /
54% on the laptop (12.8 hours old), and the laptop's next bare launch was
placed on it. Each machine was missing a different subset: the laptop had
cliushi@ and yqs@ hours old, the mini itell, qiushiyan@ and yan@. Account
and organization ids matched on all seven, so every pair described one
subscription; only the ages differed.

Goal: every surface — the board, `limits`, placement, headless placement,
the statusline chip — uses, for each account, the newest reading any of the
machines holds. The board's caption and the JSON documents name the machine
it came from; the chip and a launch's own lines show its figure and age
without the machine.
Goal: opt-in. A machine that names no other behaves exactly as before: no ssh,
no new caption, and a `peers` section left by an earlier configuration is
neither read nor reported, carried through writes as it stands.

Change: `HEADROOM_PEERS` names the other machines, as ssh reaches them
(comma- or space-separated). Claude Code's and Codex's scopes take them;
Antigravity's takes none. A name that is no ssh destination is left out of
every pull and failed by `check` against headroom's own configuration; no
other command refuses over it.
Change: `headroom readings --vendor claude|codex` (hidden) prints this
machine's own newest reading of each account, raw, as a versioned document.
Change: the rounds that already fetch — the board, `--json`, `refresh` (the
child each launch leaves, and the chip's refresh) — also pull each named
machine's document over ssh, at most once a spacing, beside their requests.
Change: the answer is kept in a `peers` section of `state.json`; each answer
adds and renews readings per account and takes none away. `accountstate`
reads it as one more candidate under the one precedence rule
(`accountstate.Newest`: newest wins, a tie at the second to the nearer
source), which a round also applies to what arrives while it runs.
Change: a new observation source, `peer`, with the machine's name: caption
"via mini2", JSON `source: "peer"`, `peer: "mini2"` (schema 8, additive).
Change: on a machine that names peers, figures are current for placement's
fifteen minutes whoever read them; on one alone, for the spacing as before.
Change: `check` prints a line per named machine: when it last answered and
how many of this machine's accounts its readings matched, or why the newest
ask failed and when the next is due, each outcome under its own time.
Change: a launch or headless placement leaves a refresh when a pull is due,
and the board schedules a round for one, even with nothing of its own to ask.

Boundary: nothing is pulled but what the other machine's headroom already
holds. No vendor request, no token, no Keychain item crosses between
machines; `readings` reads disk alone and answers where the login Keychain is
sealed, as it is over ssh.
Boundary: a reading travels one hop. `readings` prints only this machine's
own (its stored fetches and Claude Code's caches), never one it pulled, so
nothing returns to where it came from under a second machine's name.
Boundary: a reading is matched to an account by the identity the ledger keys
on (`uuid:` keys) and passes the same parse and response identity check a
reading of this machine's own does. A dir known only by name has none.
Boundary: `limits` and launches still read disk alone; only fetching rounds
pull.

## Why not the alternatives

- **Poll more often.** The stale machine has no usable token; asking sooner
  asks nobody.
- **headroom refreshes tokens.** Writes vendor login state, which headroom
  never does, and races Claude Code's own refresh-token rotation.
- **Background sessions to keep tokens alive.** Spend quota and write
  sessions to buy a figure another machine already holds.
- **Share `state.json` through twin's Unison.** Hourly is slower than a
  five-hour window moves, and one file under two machines' locks is not
  under one lock.
- **One machine as the hub.** Neither machine alone holds a current reading
  of every account; the union does.

## Design

- **The section.** `peers` → `{<machine>: {attempt_ms, generation,
  pulled_ms, failed_ms, error, readings: {<key>: {fetched_at_ms, body}}}}`.
  Read only by a store whose scope names peers; under one that names none it
  is carried through writes as it stands, as an older binary carries it.
  Disposable like the placements: an unreadable section is reported, read as
  empty, and set aside at the next pull. Readings are merged per key, newer
  over older, of only the readings a reader would take — `uuid:` keys, a body
  the store keeps, stamped neither past the clock skew allowed nor older than
  the ledger's thirty days — so a reading stamped too far ahead never stands
  over a later valid one; at most 256 per machine. The read applies the same
  bounds. An answer clears the failure it follows, and the generation of the
  newest outcome says whether an ask has one yet, so which outcome came last
  never rests on a clock. A machine
  nobody has asked in thirty days is swept at a pull, by age, never by who
  the caller's configuration names; a home that names no peer at all carries
  the section as it stands.
- **The pull.** `state.ClaimPull` is a test-and-set under the store's lock so
  that of the surfaces asking at once only one opens a connection; it hands
  back a generation, and `RecordPull` drops an outcome whose generation is
  not the newest ask's, as `Complete` does a stale fetch. The next pull is due
  a spacing later, or five minutes after a failure (a laptop asleep costs
  each ask a connection timeout) — `Snapshot.NextPull` states the same rule
  for the launch gate and the board's schedule, and says not soon on a
  document no claim can write (a newer headroom's, or one unreadable), as
  `NextEligible` does. ssh runs with
  `BatchMode=yes`, `ConnectTimeout=3`, `-T`, and the remote command puts
  `~/.local/bin` (where `make install` puts headroom) ahead of PATH; the ask
  is bounded at eight seconds, and a child left holding the output pipe has a
  second more to let go (`WaitDelay`); the answer is bounded at 4 MiB. A pull cut short by the round's own
  end records nothing. A failure keeps the readings and records why under
  its own time. This machine failing to claim or record a pull is a `peers`
  problem of the round, in the JSON documents.
- **The answer.** `readings` prints a list, empty when nothing is known; a
  document with no list, of another schema or for another vendor is refused
  by name. Exit 2 is diagnosed as a build without the command only on the
  refusal such a build prints (`unexpected argument readings`); any other
  exit says the remote's first stderr line.
- **The read.** `accountstate.Select` considers this machine's stored fetch,
  Claude Code's cache, then each named machine's reading, under
  `accountstate.Newest`. A machine the scope does not name is never read, so
  turning the variable off returns the board to this machine's own figures at
  once.
- **In a round.** A round that pulls turns every arrival — a request's
  answer, a pull that landed — into figures in one place, in order: a
  selector goroutine reads the store after each and chooses the affected
  accounts' figures by `Select`, with any answer of this process the store
  could not record as one more candidate. Each choice is made from a store at
  least as new as the one before it, so the last to land is the newest, and
  `Apply` only shows what it is handed. A round that pulls from nobody shows
  each answer as it lands, as on `main`. A pull never touches the attempt
  axis, which is about this machine's request. Plan and label are rebuilt
  from the account each time the figures change (`Facts.Show`), so a reading
  naming no plan never keeps another's.
- **Freshness.** `accountstate.FreshFor(scope)`: on a machine that names
  peers, `max(spacing, placement.StaleAfter)` — 15 minutes — for every
  source; on one alone, the spacing. What makes figures current is their
  age, never who read them: a newer reading of an account this machine cannot
  ask about arrives at the cadence of whichever machine drives it, every few
  minutes while it is used there. First built as two spacings for pulled
  readings only, which dogfood showed too tight (mini2's reading of an
  account in use was 3 minutes old, and the row read "stale" with a
  run-a-session errand), then as fifteen minutes for pulled readings only,
  which the review showed gave two meanings to "current" on one board.
- **Captions.** "via <machine>" names a pulled reading. On a machine that
  pulls, an aged-out token is not captioned while the figures on show are
  current, whoever read them: the errand buys a reading, and a current one is
  on show. A machine on its own captions it as it always has. A drift marker on
  a pulled reading sends to `headroom check on <machine>`, since check
  audits the responses its own machine stored.

## Review revisions

Round 1 (Astra, `review-r1`) found, and the branch now fixes: a late pull
completion overwriting a newer answer (generation); the board and placement
able to settle on different readings (one rule, `Newest`, applied in both
places, and a round re-selecting from the store after a pull); a pull's
write ending the conservative silence an unreadable ledger owes (the ledger
section is written undecodable until the first claim rebuilds it — a hole
`Place` shared on `main`); launches and the board scheduling only on local
eligibility; `make test-pty` able to reach the real peer; output caps
bypassed by `bytes.Buffer`'s promoted `ReadFrom`, and a held pipe outliving
the bound; an incomplete export erasing good readings (merge per key, and an
explicit list on the wire); a bad `HEADROOM_PEERS` name refusing every
command, and a leftover section reported on a machine that names no peer;
`check` dating an old failure to a new ask; every exit 2 diagnosed as an old
build; a peer's drift sent to a check that cannot see it; the freshness
window depending on the source.

Round 2 (`review-r2`) found the round-1 fixes partly integrated and four
regressions, all fixed: two pulls' store selections could land out of order
and roll the board back, and the live path's `Newest` let a stored reading
stamped ahead outlast the answer that replaced it (one selector goroutine,
and `main`'s live path for a round that pulls from nobody); a future-stamped
reading admitted by the merge hid later valid ones (the merge admits only
usable readings); an answer after a clock step back left a failure standing
(an answer clears the failure; the outcome's generation says pending). Also:
`NextPull` on an unwritable document, the unpaired section left undecoded,
the plan kept across readings, the bound's wording, the token caption on an
unpaired machine, the setup skill's refresh step and "via" wording, and
sharper pins for the board mark, headless placement, both peers' order, the
unrecorded answer, the exporter's onward pull, an absent vendor's `[]`, the
pull's own deadline and check's per-group lines. Rebutted: legacy failure
records from the branch's first commit — that format was never installed,
and the one record written in it, by the dogfood run, holds no failure.

## Verification

- `internal/state/peers_test.go`: one surface pulls at once and `NextPull`
  names the next; the failure retry; a failure keeps the readings under its
  own time; a late completion dropped; an answer renews and never erases;
  admission limits; clock anomalies; an unreadable section set aside; a home
  naming no peer leaves the section alone; a pull before the first claim
  keeps the ledger quiet; age-out.
- `internal/accountstate/peers_test.go`: newest wins across machines, a tie
  to this machine's own; a machine the scope does not name is not read; one
  freshness window per configuration; `OwnReadings` is this machine's
  newest, Claude Code's cache included, never a pulled reading.
- `internal/round/peers_test.go`: after a pull the board shows the store's
  choice and the attempt stands; every arrival order comes to one answer,
  a clock ahead and an unrecorded live answer included; opt-in and spaced;
  failures said where they belong.
- `internal/peers/peers_test.go`: schema, vendor and missing-list refusals;
  the newer of a repeated account; ssh arguments; each failure said in terms
  of what to do; size and time bounds; a pull cut short records nothing.
- `internal/launchop/refresh_test.go` and `internal/app/accounts_test.go`: a
  due pull leaves a refresh and brings the board's round forward.
- `internal/check/peers_test.go`: each outcome's line; both groups through
  `Run`; a bad name fails against headroom's own configuration.
- `internal/app/launch_model_test.go`: a newer pulled Fable figure excludes
  the account from a Fable launch.
- `internal/app/codex_test.go`: a pulled Codex reading naming another user
  is never shown.
- `internal/app/peers_bin_test.go`: two homes, the real binary, a stub ssh
  running `readings` under the other home from `~/.local/bin` — `refresh`
  pulls, the other machine runs no Keychain, vendor or ssh, `limits` shows
  `peer`/`mini2`, `readings` does not echo it, and the same home without the
  variable shows its own.
- `test/pty/run.sh`: an ssh stub ahead of the real one, and the run fails if
  any case reaches it.
- Dogfood, 2026-11-27, the branch build against both machines' live homes
  (parked on the mini outside PATH, reached by an ssh wrapper): the mini
  answered `readings` over ssh with its Keychain sealed; one `refresh` on the
  laptop pulled its seven readings in 0.7s; the laptop's board then showed
  cliushi@ at 82% / 91% Fable "observed 5m ago · via mini2" where it had shown
  57% / 54% thirteen hours old, and its next-launch mark moved off cliushi@.
  The same home with the variable unset showed its own figures as before.
  The installed (pre-branch) build kept rewriting the laptop's `state.json`
  afterwards and carried the `peers` section through untouched. A first pull
  through a broken wrapper failed, was recorded with its reason, and held the
  next pull off for five minutes, as designed.
