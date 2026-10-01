# Automatic placement — a bare launch chooses the least-loaded account

Status: built on the `auto-placement` branch, 2026-10-01, after one consult
round (envoy job `consult-r1`, Codex); kept as the dated record of the
decision. The design as it stands lives in `DESIGN.md` § Automatic placement —
read that, not this, for what is true today. Written against `main` at
81c3045, Claude Code 2.1.286, codex-cli 0.155.1, macOS. Every measurement
below was taken on the owner's laptop on that date. Obligation 16 — a day of
real use — is not yet met; § Delivery — What the build changed lists where
the build departed from the first text.

## Summary

Current: a bare `headroom launch` starts the vendor on the one account `.current` names; only the board's enter moves it.
Current: the owner holds six Claude Code subscriptions and spreads sessions by hand, by typing a named launcher per session.
Current: nothing records which account a launch used, so the spread cannot be examined afterwards.

Goal: with automatic placement on, each new session starts on the account with the least five-hour load among the accounts that are not near any limit, and says which before the vendor starts.
Goal: current five-hour usage is the main measure; busy sessions and launches too recent to show in the figures are added to it as load on its way.
Goal: an account near a weekly limit is set aside, and among equally loaded accounts the one with the most weekly room is chosen.
Goal: every launch leaves one line in a log that holds the inputs the choice was made from.

Change: `.current` records either an account (pinned, today's meaning) or the word `auto`; a bare launch follows it.
Change: `launch` gains `--auto`, `--last` and `--dry-run`; a new `launches` command reads the log, and a new `refresh` command runs one unattended round.
Change: choosing an account and recording the choice are one operation of the state store, so launches that start together see each other.
Change: every launch, in any mode, records its placement, so a named launch counts as load for the automatic ones.
Change: in auto mode a launch that names a session by id goes to that session's account while it is not near a limit; a first placement of a named session is recorded as a re-home.
Change: an automatic launch starts a detached refresh of the usage figures for the next launch, and never waits for the network itself.
Change: the session registry reader decodes the vendor's status field; `check` asserts it is there.
Change: the board marks the account the next launch would take, and `a` turns auto on; `--json` becomes schema 6 with a `mode` per vendor.
Change: headroom writes a third file of its own, `launches.jsonl`, which no routing decision reads.

Boundary: placement happens once, at launch. A running session is never moved, and no request is proxied.
Boundary: placement spreads new sessions. It does not promise that a session started on an account will not later exhaust it.
Boundary: pinned mode routes as today; auto is opt-in and an upgrade changes no one's routing.
Boundary: `--continue` and a bare `--resume` are placed like new sessions until the vendor's choice of session is verified (P5).
Boundary: Codex gets the mode and the log, without busy-session evidence and without session routing.
Boundary: use of the same accounts from another machine is visible only through a usage fetch.
Risk: the step size and the thresholds are policy constants, chosen and not measured (P8).
Unverified, each with a fallback that keeps the design: whether a print-mode session registers as live (P3), the registry status vocabulary beyond `busy` and `idle` (P2), and that prompt caches are not shared between accounts (P9).
Decided: an account that cannot be asked is tried, not rationed (§ Delivery — Decisions).
Open: no decision waits on the owner.

Where: § Behaviour describes eleven situations; § Design carries the rule, the store operation, the log and the evidence; § Verification numbers the obligations; § Delivery holds the phases.

## Intent

### Goals

- A person who turns auto on types the same bare launch as before and gets
  a session on an account with room, without consulting the board first.
- The choice is explained where it is made: one stderr line before the
  vendor starts names the account, the mode and the figures that decided it.
- The choice can be seen beforehand: the board marks the account the next
  launch would take from its snapshot, and `headroom launch --dry-run`
  prints the whole table.
- Launches that start within the same second (an agent fanning out several
  sessions) each see the ones before them.
- A session resumed by id keeps its account, so its prompt cache and its
  `/rewind` checkpoints stay usable, unless that account is near a limit.
- A named launch, a pin and `--last` override the mode.
- Every launch headroom performs is recorded with the inputs available at
  that moment.

### Non-goals

- **Routing individual requests.** That needs a proxy holding OAuth tokens
  for inference traffic. headroom never touches inference, and the vendors
  restrict these credentials to their own clients (DESIGN.md § Status).
- **Moving a running session.** The remedy for a session that runs out
  mid-flight stays the session picker's `x`.
- **Forecasting consumption.** The rule orders accounts by what is known
  now. It holds no model of how fast a session spends.
- **Spending the weekly quota that resets soonest first.** Several tools
  rank on it (P11). It concentrates sessions on one account, and its gain
  appears only when total demand nears total capacity.
- **Choosing by the model a session will use.** The launcher does not know
  it, so every weekly row counts, including a model-scoped one the session
  may not spend against.
- **Weighting by plan size.** Percentages are relative to each plan. All of
  the owner's accounts are on one plan (P10).
- **Session routing for Codex.** Codex's own resume picker reads a shared
  store and headroom has no owner model for it.
- **A usage time series.** The log holds every account's figures at each
  launch. It cannot show a peak between launches or what a session spent.
- **The owner's shell wrappers and the steward host.** `x` needs no change
  for auto mode. A launcher for `--last` or `--auto` is a new function in
  the owner's dotfiles, because whatever follows `x` goes to the vendor.
  The steward runs under its own home with one login.

### Vocabulary

**Mode** is what a bare launch does: **pinned** to one account, or
**auto**. **Placement** is the choice of an account for one launch. An
**observation** keeps its existing meaning: one usage response with the
time it was taken. An observation older than fifteen minutes is **stale**,
and its rows are lower bounds. The **session window** is the vendor's
shortest limit: Claude Code's `session` row (five hours), and the shortest
window of Codex's main rate limit. A **busy session** is a live session
whose registry status is `busy`. A **pending placement** is a launch made
on the account in the last fifteen minutes. **Load** is
a count of steps: one per ten points of session-window usage, one per busy
session, one per pending placement. An account is **near a limit** when
any of its rows is at 80% or above.

## Tenets

1. In auto mode every usable account is a correct answer. Missing
   bookkeeping or missing figures therefore degrade the choice and never
   refuse the launch. Corrupt routing state still refuses: an unreadable
   `.current` says nothing about what the person chose. Held by:
   situations 5 and 10, and obligations 3 and 7.
2. The five-hour load decides among accounts that are not near a limit. A
   weekly limit can set an account aside; it never hides a five-hour
   difference between two others. A rule that folds both into one figure
   lets the larger weekly number absorb every new launch, and the launches
   then pile onto one account. Held by: the rule in § Design — API and
   obligations 1 and 4.
3. A figure that cannot be refreshed is a lower bound, used and labelled as
   one, never recorded as measured room. The idle accounts are the ones
   that cannot be asked, and avoiding them would recreate the pile-up this
   exists to end. Held by: situations 4 and 5, and obligation 1.
4. Choosing and reserving are one operation with one owner. A choice
   computed before the lock and recorded after it leaves the gap in which
   two launches both see the account as empty, and a transaction handed to
   callers makes every launching surface responsible for the ordering.
   Held by: obligation 4.
5. Where a session is routed, which account drove it, and how much load is
   on its way are three facts with three lifetimes. Merging them lets a
   reservation that expires delete a routing decision, or an intent pass
   for evidence. Held by: situation 7 and obligation 6.
6. The log explains decisions. It is never an input, and it cannot show
   what a different rule would have cost. Held by: obligation 8.

Unless you know better ones.

## Behaviour

### 1. Turning auto on and off

Today: enter on a board row records that account in `.current` and exits.

After: `a` on the board records `auto` for the visible page's vendor,
prints what a bare launch now does, and exits. Enter on a row pins that
account, as today, and leaves auto. `headroom launch --auto --remember` is
the scriptable spelling of `a`, as `--account <name> --remember` is of
enter. Nothing turns auto on by itself: an existing `.current` keeps
naming its account after an upgrade, and an absent one still means the
primary.

### 2. A bare launch in auto mode

After: the launch reads what is on disk about every account, chooses,
records the placement, starts a detached refresh for the next launch,
prints one line and replaces itself with the vendor. It makes no network
request of its own.

```
headroom launch: cliushi@planlab.ai · auto · 5h 1% · week 3% · load 0 (next: qiushi.yann@gmail.com)
```

Mechanism: the rule in § Design — API; the order of steps in § Design —
Wiring.

With the owner's figures at 09:56 UTC on 2026-10-01 (P10):

| account | session window | busy | load | tightest weekly |
|---|---|---|---|---|
| cliushi@planlab.ai | 1% | 0 | 0 | 3% |
| qiushi.yann@gmail.com | 0%, 36 h old | 0 | 0 | 5%, 36 h old |
| qiushi (primary) | 5% | 0 | 0 | 40% |
| qiushiyan@planlab.ai | 4% | 1 | 1 | 31% |
| yqs@planlab.ai | 0% | 1 | 1 | 42% |
| yan@planlab.ai | 3% | 2 | 2 | 22% |

Three accounts have load 0. The launch goes to cliushi, which has the most
weekly room of the three.

### 3. Several launches at the same moment

After: each launch takes the store's lock in turn and sees the placements
recorded before it. Four launches started together against the table above
go to cliushi, the gmail account, the primary, and cliushi again: after
three, every account has load 1 or more, and cliushi has the most weekly
room among those at load 1.

The promise is that each launch goes to the least-loaded account at its
turn. Launches land on distinct accounts for as long as accounts with
equal load remain.

Mechanism: the store's placement operation (§ Design — Structure).

### 4. An account that cannot be asked

Observed: at 09:30 UTC four of six accounts could not be asked, because
their access tokens had aged out and only the vendor refreshes them (P6).
Their figures were 17 to 40 hours old. Three carried session rows whose
reset had passed; the fourth had never started a window.

After: a stale observation's rows count as lower bounds. A row whose reset
has passed counts as 0, because the window it described has ended. The
account is then ordered like any other, so an idle account is tried
first. When it is chosen, the line says its figures are old:

```
headroom launch: yan@planlab.ai · auto · figures 40 h old · load 0
```

The placement adds one step of load to the account for fifteen minutes,
so a second launch does not follow it there on the same unverified
figures. Starting the session refreshes the token, and the
next launch's refresh can then ask.

If the account was in fact used elsewhere and is exhausted, that session
meets the limit at its first prompt. This is the cost of trying an account
that cannot be asked (§ Delivery — Decisions).

The board keeps showing a rolled-over row as unknown. A display must not
print a low percent for a window that has ended; a placement may treat it
as a bound because it says so and counts its own launch against it.

### 5. An account with no observation

After: an account headroom has never observed is treated as an account
with stale figures of zero: tried, labelled as never observed, and loaded
by its own pending placement. A row whose percent does not parse makes the
account near a limit, because nothing bounds it.

When no account has any observation, the launches rotate: least recently
placed first.

### 6. Every account near a limit

After: when every candidate is near a limit, the launch goes to the one
whose highest row is lowest, and among equals to the one whose highest
row resets first. The line says so:

```
headroom launch: yan@planlab.ai · auto · every account is near a limit · week 84%, resets in 1.2 d
```

An account the vendor reports as blocked is never chosen. When every
account is blocked, logged out or fails launch preparation, the launch
refuses and names each reason.

### 7. A launch that names a session

Today: `headroom launch -- --resume <id>` runs on `.current`, whichever
account drove the session before.

After, in auto mode, for Claude Code: when the arguments after `--` carry
`--resume <uuid>`, `--resume=<uuid>`, `-r <uuid>` or `--session-id <uuid>`,
the launch resolves that session's owner with the existing resolver. An
owner that is usable and not near a limit takes the launch; the line says
`owner`, and nothing is written. Otherwise the session is placed like a
new one, and the placement is recorded as a re-home of that session, the
record the picker's `x` writes. The line says the session moved, or that
it had no known account.

This is what keeps a headless agent on one account across turns. A
print-mode session leaves no prompt history (P4), so without the re-home a
session resumed turn after turn would be placed afresh each time and lose
its cache each time.

`--continue`, `-c` and a bare `--resume` name no session headroom can
identify (P5). They are placed like new sessions, nothing is recorded
about a session, and the line says the session was not identified. The
arguments reach the vendor unchanged in every case.

Pinned mode keeps today's behaviour.

### 8. Overrides

After: `headroom launch --account <name>` starts on that account in either
mode and changes neither the mode nor the pin, as today. `--auto` places
one launch automatically while the mode stays pinned. `--last` starts on
the account of the newest recorded placement; it refuses when none is
recorded or that account is gone. `--auto`, `--last` and `--account`
exclude one another. In pinned mode the launch line says so, so a pin that
was forgotten is visible at every launch:

```
headroom launch: qiushiyan@planlab.ai · pinned (a on the board turns auto on)
```

Turning auto on replaces the pin. Pinning again is enter on the board.

### 9. Seeing the choice before and after

After: in auto mode the board shows no `← current`. The header says bare
launches are automatic, and the row the rule would choose from the page's
snapshot carries `← next`, in both layouts and in the one-shot print. The
marker is advice: a launch reads the disk again and counts placements made
since.

`headroom launch --dry-run [--auto | --last | --account <name>]` reads the
disk, runs the rule and prints one row per account (figures, their age,
busy sessions, pending placements, load, tightest weekly, and why an
account was excluded). It records no placement, writes no log line,
starts no refresh and does not exec.

`headroom launches` prints the newest log records, one line each: time,
account, mode and reason, load, and the runner-up. `--json` emits the
records unchanged.

`headroom resolve` without a name refuses in auto mode and points to
`--dry-run`. A name still resolves.

### 10. Bookkeeping that cannot be read or written

After: when the store's lock is busy past its wait or the file was written
by a newer headroom, the launch chooses from what it could read, says on
stderr that the placement was not recorded, and starts. A placements
section that does not decode is set aside the way the store sets aside any
section it cannot read: the launch chooses with no placements counted,
starts, and its own placement begins the section again, recorded and
without a warning. The other sections of the file are untouched. A log
line that cannot be written is one stderr line and nothing more. A refresh
that cannot be started is silent.

One launch does depend on the bookkeeping: the picker's `x`, which moves a
session to another account. Its re-home is what routes the session's next
turn, so when the re-home cannot be written the move is refused, and
nothing is recorded, logged or started.

`.current` keeps its strict reading. Empty, unreadable, or naming neither
an account nor `auto`, it refuses the launch. A discovered account named
`auto` makes the word ambiguous, and the launch refuses and names the
directory.

### 11. Codex, and the session picker

After: `headroom launch --vendor codex` follows Codex's own `.current`,
which can also say `auto`. Load is computed from Codex's session window
and pending placements. A blocked allowance excludes the account. There
are no busy sessions to count and no session routing: `-- resume` is
placed like a new session.

In the session picker, enter resumes on the owner as today. Where it fell
back to the current account (no evidence, or an owner that no longer
exists) it falls back to a placement in auto mode. `x`, which today moves
the session to the current account and re-homes it, moves it in auto mode
to the least-loaded of the other accounts.

## Design

The existing code is a base this extends. Nothing is reshaped first.

### Structure — responsibilities

- **The mode.** Owner: the accounts package. Protects: one routing fact,
  read strictly, that refuses on corruption. Today `Set.Select("")` reads
  `.current` and returns an account (`internal/accounts/accounts.go`).
  After: the set answers "what does a bare launch do" with pinned and an
  account, or auto. `Select` with a name is unchanged. Recording auto goes
  through the same atomic write as `SetCurrent`.
- **Busy sessions.** Owner: the sessions package. Protects: one reader of
  the vendor's registry record, and liveness by pid and start instant.
  Today `ReadRegistry` decodes `sessionId`, `pid` and `startedAt`
  (`internal/sessions/registry.go`). After: it also decodes `status`,
  carried verbatim and tagged present, absent or wrong type. Only `busy`
  counts as busy; every other value is carried to the log as it was read.
- **The session window.** Owner: the usage package. Protects: which row is
  the short window is decided beside the parser that knows the vendor's
  vocabulary, and nowhere else.
- **The rule.** Owner: a new leaf package, sketched as
  `internal/placement`. Protects: the rule is a pure function with no
  clock, file or network of its own. It takes the candidates, the pending
  placements and the time, and returns the choice with every candidate's
  counted figures.
- **Placing.** Owner: the state package. Protects: choose-and-reserve is
  one operation, as `Claim` is for requests. Today the store has `accounts`
  and `sessions` sections and no exported transaction
  (`internal/state/store.go`). After: one concrete operation takes the
  candidates and the intent (automatic, forced to an account, or last),
  and inside one locked section reads the placements, drops the superseded
  ones, calls the rule, and records the result. The state package imports
  the rule; no caller passes a function in.
- **Session routing.** Owner: the sessions package and the store's
  existing re-home records. After: a placed session whose id is explicit
  is re-homed by the same record the picker writes. A record younger than
  ten minutes is not swept for a missing transcript, because a first
  turn's transcript does not exist yet when its record is written.
- **Refreshing for the next launch.** Owner: the refresh package,
  unchanged, run in a detached child. Protects: every request still
  passes the claim, and every response is recorded by a process that
  lives to record it.
- **The log.** Owner: a new leaf package, sketched as `internal/launchlog`.
  Protects: append-only, one line per launch, its own lock, read only by
  the `launches` command.
- **The launch surface.** Owner: `internal/app/launch.go`. Protects: the
  child environment stays a total function of the validated account,
  through `launch.Prepare`, whichever way the account was chosen. One
  parser reads launch intent from the arguments and leaves them untouched.
- **The board and `check`.** Owners: `internal/app/accounts.go` and the
  check package. After: the marker and the key; assertions on the registry
  status field and on headroom's own new state.

### Design it twice — where the mode lives

- *Constraint: the caller says what it wants.* The shell function passes
  `--auto`. Rejected: shell functions are loaded at shell start and live
  for weeks, so panes would disagree about what `x` does until each is
  restarted, and envoy's launcher string and the session picker would each
  need the same edit (DESIGN.md § The launch surface, on the two layers'
  lifetimes).
- *Constraint: no existing file changes meaning.* A second file, `.mode`,
  beside `.current`, which would keep the pin while auto is on. Rejected:
  two files can say "auto" and "this account" at once, and every surface
  that marks the current account would have to read both to avoid marking
  an account no launch targets.
- *Constraint: one routing fact.* `.current` holds an account name or the
  word `auto`. **Chosen.** One strict read, one failure policy. The costs
  are a reserved word and that turning auto on forgets the pin.

### Design it twice — the rule

- *Constraint: the answer is one number.* Room: 100 minus the highest row,
  with expected usage added to the session window (P11: claude-swap, ccs).
  Rejected: this spec's first draft chose it. With session usage at 0 and
  weekly usage of 40, 50, 60 and 70 on four accounts, four launches in a
  row all go to the first account, because ten points added to a session
  window of 0 never reach the weekly 40 that decides its room.
- *Constraint: waste no weekly quota.* Priority tiers, the session window
  as a gate, ranking by the weekly reset that comes soonest (P11:
  claude-rotate, teamclaude, better-ccflare). Rejected: it concentrates
  sessions on one account by design.
- *Constraint: each limit does one job.* Five-hour load ranks, a weekly
  threshold sets accounts aside, weekly room breaks ties. **Chosen.** A
  placement always raises the chosen account's rank key, so launches
  spread. Weekly usage evens out across accounts because it decides every
  tie. The cost is three constants (P8).

Load counts session usage in steps of ten points so that a one-point
difference in a figure does not outrank a forty-point difference in weekly
room, and adds busy sessions and pending placements in the same unit so
that an account at 9% with five busy sessions is not preferred to one at
11% with none.

### Design it twice — the placing operation

- *Constraint: storage knows no policy.* The rule runs outside the lock; a
  `TryReserve(revision, choice)` rejects a stale decision and the caller
  retries. Rejected: revision checks and retry behaviour enlarge the
  store's interface for one local use.
- *Constraint: everything is always current.* A resident coordinator owns
  placement and refresh. Rejected: a process lifecycle and IPC for a
  decision made at launch.
- *Constraint: one operation owns the decision and the reservation.* A
  concrete store operation calls the rule under its lock. **Chosen.** The
  store gains a domain operation and exports no transaction.

### API

**The command surface.**

```
headroom launch [--vendor <v>] [--auto | --last | --account <name>] [--remember] [--dry-run] [-- args]
headroom launches [--vendor <v>] [-n <count>] [--json]
```

`--auto --remember` records auto. `--dry-run` with `--remember`, and
`--last` with `--remember`, are usage errors.

**What a row counts as.** First match wins.

| the row | counts as | basis in the log |
|---|---|---|
| percent does not parse | near a limit | `bad` |
| reset has passed | 0 | `ended` |
| observation is stale | its percent, a lower bound | `stale` |
| otherwise | its percent | `observed` |

An account with no observation has no rows and basis `none`. An
observation that reports no limit rows counts as load 0 with nothing near
a limit.

**Load**, in whole steps: the session window's counted percent divided by
ten, rounded down; plus one per busy session; plus one per pending
placement. A session that is both busy and pending counts once. A pending
placement is one recorded on the account less than fifteen minutes ago,
whether or not its process still runs and whatever was observed since: an
observation taken a second after a launch reflects none of what that
session will spend.

**The choice.** Candidates are the accounts that pass launch preparation
and that no positive evidence rules out: an identity document that parsed
and names nobody, a refresh token demonstrably expired, or a block the
vendor reported. A credential that could not be read rules nothing out.
The vendor's `auth status` probe is not run at launch, and credentials are
read only when the rule will choose.

1. Forced (`--account`, a pin, `--last`): that account, if it is a
   candidate; otherwise refuse, as today.
2. A launch that names a session whose owner is a candidate not near a
   limit: the owner.
3. Among candidates not near a limit: the least load. Ties go to the
   lowest tightest weekly row, then to the least recently placed, then to
   board order.
4. If every candidate is near a limit: the lowest highest row, then the
   soonest reset of that row.
5. No candidates: refuse.

**The constants**, in one place and named in the rule's version: a step of
ten points; one step per busy session and per pending placement; 80% as
near a limit; fifteen minutes for staleness and for a pending placement.

**The `placements` section of `state.json`** (names are sketches):

```json
"placements": {
  "recent": [{"account": "uuid:…", "name": "cliushi@planlab.ai", "pid": 4242, "at_ms": 1790848000100}],
  "last":   {"uuid:…": {"name": "cliushi@planlab.ai", "at_ms": 1790848000100}}
}
```

`recent` holds placements younger than fifteen minutes. `last` holds the
newest placement per account for thirty days: its newest entry is what
`--last` reads, and each account's is the tie-break. Load is keyed by the
ledger key, because two directories on one account share one quota. The
pid matches a placement to the registry record of the session it became. The section is disposable: when it does not decode it is set
aside and treated as empty. A binary from before this change carries it
through its writes untouched.

**One log line** (`launches.jsonl` under the vendor's accounts root):

```json
{"v":1,"at":"2026-10-01T10:02:11Z","vendor":"claude","pid":4242,"cwd":"/Users/…",
 "mode":"auto","reason":"least-load","rule":"load-1","chosen":"cliushi@planlab.ai",
 "runner_up":"qiushi.yann@gmail.com","session":"","recorded":true,
 "candidates":[{"name":"qiushi","eligible":true,"near_limit":false,
   "observed_at":"2026-10-01T09:56:40Z","source":"headroom_cache",
   "limits":[{"kind":"session","label":"5h session","percent":5,"resets_at":"…","session":true,"counted":5,"basis":"observed"}],
   "statuses":["idle"],"busy":0,"pending":0,"load":0,"weekly":40,"last_placed_at":"…"}]}
```

`mode` is how the account came to be decided: `auto`, `pinned`, `named`,
`last` or `picker`. `reason` is the rule's word: `pinned`, `named`,
`picker`, `last`, `least-load`, `near-limit`, `owner`, `moved`,
`rotation`. A record that did not reach the placements section carries
`recorded: false` and a `problem`. An append is one write. When the
file exceeds 8 MB, the appender takes the log's own lock without waiting
and, if it gets it, rewrites the file without lines older than 180 days.
The state lock is never held for the log.

**`--json` schema 6.** Adds `mode`, keyed by vendor: `"pinned"`, `"auto"`,
or `""` when `.current` cannot be resolved. Under auto, `current` for that
vendor is `""` and no account carries `current: true`.

### Wiring — the changed paths

**A launch.**

- Today: parse flags, discover, `Select`, `launch.Prepare`, optionally
  record `.current`, print notices, exec (`internal/app/launch.go`
  `runLaunch`).
- After:
  1. Parse flags and read launch intent from the arguments. Resolve the
     mode.
  2. Discover, and run `launch.Prepare` for every account. A failure
     excludes that account and is said on stderr. Preparation reads no
     network and starts no process.
  3. Read facts from disk, credentials, and the registry of every Claude
     Code account with one process sample per pid.
  4. Resolve the named session's owner, if an id was given.
  5. Call the store's placement operation with the candidates and the
     intent. On a lock, decode or schema failure, run the rule over the
     candidates alone and record nothing.
  6. Record a re-home when a session with an explicit id was placed
     rather than followed. With `--remember`, record the mode.
  7. Append the log line. In auto mode, start the detached refresh.
  8. Print the notices and the launch line, then exec the prepared target.

  A pinned or named launch takes the same path with a forced choice, so
  its placement is recorded and logged, and it starts no refresh.

  The placement's pid is the launching process's own, which the exec keeps
  (P1). A recorded choice survives a failed exec, as a recorded pin or
  re-home does today, and the error says so.

**The detached refresh.**

- Today: only the board, `--json` and `check` fetch
  (`internal/refresh/refresh.go`).
- After: an automatic launch starts a copy of headroom that runs one
  refresh round for the vendor, with no terminal, in its own session, and
  exits. It is started only when some account's next-eligible instant has
  passed. The round is the existing one: the claim authorizes, the
  completion records, and nothing is abandoned, because nothing waits on
  it.

**A session's owner.**

- Unchanged: a verified live registry claim, else the newest of the
  explicit re-home and each account's newest history prompt
  (`internal/sessions/sessions.go`). A placement adds a re-home record and
  no new kind of evidence.

**The session picker.**

- Today: `resumeAccount` chooses the owner, then the current account; `x`
  chooses the current account and re-homes
  (`internal/app/session_actions.go`).
- After: where it reads the current account it asks the mode. Under auto
  it calls the same placement operation, excluding the present owner for
  `x`. It logs with reason `picker`.

**The board.**

- Today: enter records the selected account and exits
  (`internal/app/accounts.go`).
- After: `a` records auto and exits. A page in auto mode runs the rule
  over its facts after each round and marks that row. It records nothing.

### Premises

**P1. The launch process becomes the session process.** Settled. Basis —
`launch.ExecPath` calls `syscall.Exec`, and measured: a live registry
record's pid (67709) is a `claude` process whose parent is the login
shell. Does not establish: a vendor build that re-executes itself.

**P2. The registry carries a status.** Settled for the field, open for its
vocabulary. Basis — measured on live records, 2.1.286: each carries a
string `status` with `statusUpdatedAt`, and `kind: interactive`. At 09:30
UTC one of five was `busy` and four `idle`; at 09:56 four of seven were
`busy`. The consult's own sample at 10:00, with process start instants
matched, saw one `shell` among six. Does not establish: the full
vocabulary, what `shell` means, or how soon the field changes when a turn
starts. Fallback: only `busy` adds load; any other value adds none and is
logged as read.

**P3. Whether a print-mode session registers as live.** Unverified. No
print-mode record was present in any sample. Fallback: its launch is a
pending placement until a newer observation covers it, whatever the
registry says.

**P4. A print-mode session leaves no prompt history.** Settled for envoy's
sessions. Basis — measured: 48 envoy runs with provider `claude`, 39
session ids, each with a transcript in the store and no decoded record in
any account's `history.jsonl`. envoy passes `--session-id <uuid>` on a
first turn and `--resume <id>` on later ones (envoy
`internal/provider/claude.go`). This is why a placed session is re-homed.

**P5. Which session `--continue` resumes.** Unverified, and therefore not
built on. Settling it means reading the vendor's selection rule or
measuring it across two projects and two accounts. Until then `--continue`
is placed like a new session.

**P6. An account that is not in use here cannot be asked.** Settled. Basis
— measured: at 09:30 UTC `headroom --json` returned `access_token_stale`
for four of six accounts; by 09:56, after the owner had started sessions
on three of them by hand, for one. The token lives about eight hours and
only the vendor refreshes it (DESIGN.md § Three axes). A stale token on
this machine says nothing about use of the account from another machine,
which holds its own token.

**P7. What keeps stored figures fresh.** Settled for this machine. Basis —
the owner's statusline refresher runs `headroom --json` about every five
minutes while a session is open. Claude Code's own usage cache was absent
on five accounts and thirteen days old on the sixth. Does not establish:
any refresher on another install, which is why an automatic launch starts
one.

**P8. The constants.** Policy, not measurement: the ten-point step, one
step per busy session, 80% as near a limit, fifteen minutes. Nothing here
estimates what a session spends, and the log cannot calibrate them (it
holds no interval data). They are changed by judgment and by what the
owner sees.

**P9. Prompt caches are not shared between accounts.** Assumed, from the
vendor's statement that caches are isolated per organization; not
measured across two subscriptions. Fallback: if a cache were shared,
following the owner would cost nothing extra and still keep `/rewind`
checkpoints, which are per config dir.

**P10. The owner's load.** Measured: six accounts, all `max 20x`. 134
distinct sessions were prompted in the last seven days; that counts
neither resumes nor print-mode sessions, so it is not a count of launches.
At 09:56 seven sessions were live on four accounts, session-window figures
ran from 0% to 5%, and weekly rows from 3% to 42%.

**P11. What other tools do.** Established from the source of seventeen
projects, read on 2026-10-01 by a background agent and not run; this
session did not verify its report. Most are per-request proxies. The most
common rule is the most room over the highest window (claude-swap, ccs,
teamclaude). Several rank by the weekly reset that comes soonest
(claude-rotate, better-ccflare, teamclaude). claude-swap serializes its
decision under a locked state file; better-ccflare penalizes an account
picked in the last 500 ms. None places at launch with a record of launches
in flight.

**P12. An older binary under `auto`.** Settled, with one exception. Basis —
`Select` reports a `.current` that names no discovered account as an error
and refuses (`internal/accounts/accounts.go`). An older binary on a
machine with an account actually named `auto` launches that account.

**P13. Cost at launch.** Measured on the built binary against the owner's
six accounts, warm, as the wall time of `launch --dry-run`, which runs a
launch's reads and its rule and then prints instead of recording and
exec'ing: 20 ms for a named launch and 39 to 49 ms for an automatic one,
which also reads six credentials, against 4 ms for the process doing
nothing. Twenty runs each. Does not establish: the cost of the locked
write and the exec that a real launch adds.

**P14. Which limit has stopped work.** Measured, roughly: among transcripts
modified in the last thirty days, vendor limit messages appear on five
days. Seven say the session limit was hit, all on 2026-09-09. Nine say
the Fable limit was reached, on 09-15, 09-17, 09-21 and 09-23. The
seven belong to two sessions and the nine to seven. Does not establish:
how many incidents these were, on which accounts, whether another account
had room at the time (transcripts carry no account), or which limit costs
more work. It does establish that messages about the model-scoped weekly
limit appeared on more days, and in more sessions, than messages about
the five-hour one, which is why a weekly row can set an account aside and
decides ties.

## Verification

Runners: `make check` and `make test-pty`. The pty harness already stubs
`claude` and `security` under a temporary home (`test/pty/run.sh`).

1. Obligation: the rule implements the row table, load and the choice.
   Observe: the rule's result over a table of candidates, at least one per
   row of each table: an ended row, a stale row, an unparseable percent,
   no observation, no limit rows, a blocked account, a busy and pending
   session counted once, ties broken by weekly, then last placement, then
   order, every candidate near a limit with different resets, no
   candidates. Session usage 0 with weekly 40, 50, 60 and 70 and four
   successive placements yields four different accounts. An account at 9%
   with five busy sessions loses to one at 11% with none. Real: the rule.
2. Obligation: the registry reader carries the status. Observe: `busy`,
   `idle`, `shell`, absent and a wrong type through `ReadRegistry`; only
   `busy` adds load; a record whose pid is alive under another start
   instant is not live.
3. Obligation: the mode is read strictly. Observe: `.current` holding an
   account, `auto`, nothing, an unknown name, and `auto` beside a
   discovered account named `auto`; absent still means the primary; a
   pinned launch produces the environment and argv it does today.
4. Obligation: choosing and reserving are one operation. Observe: N
   processes launched together against the real store and a stub vendor:
   with N accounts of equal load, N distinct accounts; every placement
   present afterwards; a held lock leaves each launch unrecorded but
   started. A short job that has exited still counts as pending until a
   newer observation or fifteen minutes; a newer observation removes it.
5. Obligation: a launch waits on no network. Observe: against a local
   HTTP server that never answers, the exec happens and the launch's own
   process made no request; the detached refresh makes one claimed request
   per eligible account and records its completion after the launcher is
   gone; no refresh starts under `--dry-run`, a pin or a named launch, or
   when every account is inside its quiet period. Substitute: the HTTP
   server, which proves the request and its recording and not the vendor.
6. Obligation: a named session follows its owner and nothing else is
   inferred. Observe: `--resume <uuid>`, `--resume=<uuid>`, `-r <uuid>` and
   `--session-id <uuid>` in the stub's recorded argv and environment; an
   owner near a limit is left and a re-home is written; an owner followed
   writes nothing; `--continue`, `-c`, bare `--resume`, and a value that
   is not a UUID record nothing and are placed like new sessions; the
   vendor's argv is byte-identical to what was passed; two sessions
   re-homed within a second both keep their records before their
   transcripts exist; verified live evidence still outranks a re-home.
7. Obligation: degraded bookkeeping never refuses an ordinary launch.
   Observe: a held lock and a document from a newer schema each still
   exec, with the stderr line and `recorded: false` in the log; a
   placements section that does not decode still execs, is replaced by
   the launch's own placement and is recorded; re-homes in the same file
   are byte-identical afterwards in all three. The picker's `x` with
   re-homes unwritable is refused and leaves no placement and no log
   line, and the next resume in the same picker is counted once.
8. Obligation: the log is complete and inert. Observe: one line per exec
   in every mode and from the picker; a launch with the log deleted, with
   a torn last line, and with the log's lock held chooses the same account
   in the same time; lines past the age bound are dropped only above the
   size bound.
9. Obligation: `--dry-run` has no effects. Observe: state file, log and
   `.current` byte-identical, no child process, no exec, no request.
10. Obligation: the board shows the mode. Observe: through the pty
    harness, `a` writes `auto` to the visible vendor's `.current` alone
    and enter pins again; with inputs frozen and no placement in between,
    the `← next` row is the account a launch then chooses. The one-shot
    print and `--compact` carry the same marker.
11. Obligation: schema 6. Observe: the document under pinned, auto and an
    unresolvable `.current`, from `--json` and from `limits`.
12. Obligation: `check` covers the new facts. Observe: FAIL when no
    running session's registry record carries a string `status`;
    INCONCLUSIVE when only some lack one (a stale record under a recycled
    pid is likelier than drift) and, naming the value, on one outside
    `busy`, `idle` and `shell`; an own-state FAIL, never vendor drift, on
    a `.current` that cannot be resolved and on a placements section that
    does not decode.
13. Obligation: Codex follows its own mode. Observe: a Codex `.current` of
    `auto` places by Codex's session window and pending placements; a
    blocked allowance excludes; Claude Code's files are untouched.
14. Obligation: the overrides. Observe: `--last` after an automatic, a
    named and a pinned launch names each one's account; it refuses with
    no placement recorded and with that account removed; `--auto` under a
    pin leaves `.current` byte-identical.
15. Obligation: the picker under auto. Observe: enter with no owner
    evidence places; `x` excludes the present owner, re-homes and logs.
16. Obligation: the real thing works. Observe: on the owner's machine, a
    day of bare launches in auto mode; `headroom launches` shows no launch
    placed on an account with more load than another candidate at that
    moment; four launches started together land on four accounts when four
    had equal load. Limit: manual. It also settles P3 by observation.

## Delivery

One PR, built as phases on one branch because each stands on the one
before.

1. **The rule and what it reads.** The placement package, the registry
   status, the session window. Obligations 1 and 2.
2. **Recording.** The placements section, the store's placement
   operation, the sweep's grace for young re-homes, the log package.
   Obligations 4, 7 and 8.
3. **Launching.** The mode in the accounts package, the flags, launch
   intent from the arguments, the detached refresh, the launch line,
   `--dry-run`. Obligations 3, 5, 6, 9 and 14.
4. **Showing.** The board marker and key, schema 6, `launches`, `check`,
   Codex, the picker. Obligations 10 to 13 and 15.
5. **Words.** `docs/REFERENCE.md`, `README.md`, `DESIGN.md` (the launch
   surface gains the mode and the third file), `CLAUDE.md` and both
   shipped skills, in the same PR. Then obligation 16.

### What the build changed

- A pending placement counts for a fixed fifteen minutes. The first text
  ended it at the next observation, and an observation taken a second after
  a launch — the detached refresh of the very next launch — would then have
  erased the load before the session had spent anything.
- The record keeps the newest launch per account for thirty days beside
  the recent ones, so `--last` and the tie-break outlive the fifteen
  minutes.
- An automatic choice excludes an account only on positive evidence. The
  first text asked for a login "shown in credential evidence", which on a
  machine whose Keychain is locked would have excluded every account and
  refused the launch — against the first tenet.
- A pinned or named launch reads no credentials: they are evidence for a
  choice the rule makes, not for one the person made.
- `refresh` is an ordinary command, because the detached round has to be
  something the binary can run.
- The session picker records every resume as load and logs it, not only
  the ones the rule placed.
- `check` fails on the registry status only when no running session
  carries one.

- Every launch prints its line, a named one included (`<account> · named`),
  and so does the picker's resume. The first text kept a named launch
  silent; a line that appears on some paths and not others is one more
  thing to remember about which path was taken.
- A launch is one operation for both callers. `headroom launch` and the
  picker build an intent and hand it to the same function, which gathers
  candidates, places, prepares, logs and announces; the store's placement
  takes the session with it, so the load and the session's re-home are
  one write. The first build gave the picker its own sequence, and a
  refused `x` left a placement and a log line behind.
- The board and a launch build their candidates with one function, so the
  `← next` row and the launch cannot judge an account's eligibility by
  different evidence.
- A figure in the launch line says what it rests on: `≥N%` for an old
  observation, `window ended` for a window whose reset has passed, `?%`
  for a percent that does not parse.
- A placements section that does not decode is replaced by the next
  placement rather than left to keep every launch unrecorded
  (Behaviour 10).
- The log's appenders hold its lock shared and its pruner holds it
  exclusively, so a record written during a prune is not lost; an append
  that follows a torn line starts on a new one.
- `check` judges the registry status over sessions verified live by pid
  and start time, the evidence routing uses, not over pids that answer a
  signal.

Not done: obligation 16. The binary is installed and the owner's mode is
still pinned, so no bare launch has yet run in auto mode on the owner's
machine; P3 is therefore still open.

Outside this repository, and therefore separate: the owner's dotfiles
(`docs/claude-accounts.md`, the comments in `claude.zsh`, and launcher
functions for `--last` and `--auto` if wanted), and the steward host.

### Decisions

**How an account that cannot be asked is treated.** Decided by the owner
on 2026-10-01: try it. Its last figures count as lower bounds, it is
ordered like any other account, the line says the figures are old, and its
own placement keeps a second launch from following on the same unverified
figures. The alternative from the consult was to keep it apart as unknown
and give unknown accounts one launch in each round of as many launches as
there are accounts. That starts fewer sessions on an account that turns
out to be exhausted, and it leaves idle accounts mostly unused until each
has been tried: on the 09:30 figures, five launches in six would have gone
to the two accounts already in use.

Open decisions: none.
