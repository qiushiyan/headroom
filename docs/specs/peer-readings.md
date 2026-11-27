# Readings from other machines — one home's figures, current wherever the account is used

Status: built on the `peer-readings` branch, 2026-11-27. Written against
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

Goal: every surface — the board, `limits`, placement, the statusline chip —
shows, for each account, the newest reading any of the machines holds, named
for the machine it came from.
Goal: opt-in. A machine that names no other behaves exactly as before: no ssh,
no new section, no new caption.

Change: `HEADROOM_PEERS` names the other machines, as ssh reaches them
(comma- or space-separated). Claude Code's and Codex's scopes take them;
Antigravity's takes none.
Change: `headroom readings --vendor claude|codex` (hidden) prints this
machine's own newest reading of each account, raw, as a versioned document.
Change: the rounds that already fetch — the board, `--json`, `refresh` (the
child each launch leaves, and the chip's refresh) — also pull each named
machine's document over ssh, at most once a spacing, beside their requests.
Change: the answer is kept in a `peers` section of `state.json`, replaced
whole by each answer; `accountstate` reads it as one more candidate, newest
wins, a tie to this machine's own.
Change: a new observation source, `peer`, with the machine's name: caption
"via mini2", JSON `source: "peer"`, `peer: "mini2"` (schema 8, additive).
Change: `check` prints a line per named machine: when it last answered, or
why the newest ask failed.

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

- **The section.** `peers` → `{<machine>: {attempt_ms, pulled_ms, error,
  readings: {<key>: {fetched_at_ms, body}}}}`. Disposable like the
  placements: an unreadable section is reported, read as empty, and set aside
  at the next pull. An older binary carries it through its writes untouched.
  A machine nobody has asked in thirty days is swept by age, never by who the
  caller's configuration names.
- **The pull.** `state.ClaimPull` is a test-and-set under the store's lock so
  that of the surfaces asking at once only one opens a connection; the next
  pull is due a spacing later, or five minutes after a failure (a laptop
  asleep costs each ask a connection timeout). ssh runs with
  `BatchMode=yes`, `ConnectTimeout=3`, `-T`, and the remote command puts
  `~/.local/bin` (where `make install` puts headroom) ahead of PATH; a pull
  is bounded at eight seconds. A pull cut short by the round's own end
  records nothing. A failure keeps the last answer and records why.
- **The read.** `newestObservation` considers this machine's stored fetch,
  Claude Code's cache, then each named machine's reading; strictly newer
  wins. A machine the scope does not name is never read, so turning the
  variable off returns the board to this machine's own figures at once.
- **Freshness.** A pulled reading is fresh for as long as placement takes
  any reading as current (`placement.StaleAfter`, 15 minutes). It is the
  newest reading of an account this machine cannot ask about itself, and the
  machine that made it asks only as often as its own surfaces run — every
  few minutes while the account is used there. First built as two spacings,
  which dogfood showed too tight: mini2's reading of an account in use was 3
  minutes old, and the row read "stale" with a run-a-session errand. While a
  pulled reading is fresh, an aged-out token here is not captioned.
- **In a round.** A pull's readings arrive as results of the accounts they
  are of and land only where they are newer than what the row holds; they
  never touch the attempt axis, which is about this machine's request.

## Verification

- `internal/state/peers_test.go`: one surface pulls at once; spacing and the
  failure retry; a failed pull keeps the last answer; only `uuid:` keys kept;
  clock anomalies; an unreadable section set aside; age-out.
- `internal/accountstate/peers_test.go`: newest wins across machines; a
  machine the scope does not name is not read; freshness for placement's
  window;
  `OwnReadings` never hands on a pulled reading.
- `internal/round/peers_test.go`: a pull lands where newest and leaves the
  attempt; opt-in and spaced; a failure recorded, not shown.
- `internal/peers/peers_test.go`: schema and vendor refusals; ssh arguments;
  each failure said in terms of what to do.
- `internal/app/peers_bin_test.go`: two homes, the real binary, a stub ssh
  running `readings` under the other home from `~/.local/bin` — `refresh`
  pulls, `limits` shows `peer`/`mini2`, `readings` does not echo it, and the
  same home without the variable shows its own.
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
