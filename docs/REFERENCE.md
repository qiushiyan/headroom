# headroom reference

Commands, the model behind them, shell integration, configuration and the
developer loop. The [README](../README.md) is the user-facing story; this is
the rest. The mental model and the reverse-engineered vendor contracts are in
[DESIGN.md](../DESIGN.md); the architecture for contributors in
[CLAUDE.md](../CLAUDE.md).

## Commands

`--vendor <claude|codex>` defaults to `claude` on the commands that act on
one account — `launch`, `resolve`, `accounts add`, `accounts remove` — so an
invocation without it means Claude Code. The commands that report — the
board, `--json`, `limits`, `launches` — and `refresh` cover every vendor
present on the machine and take `--vendor` for one. `check` and `sessions` take no `--vendor`. A command
naming a vendor this machine does not have fails saying so, except
`accounts add`.

- **`headroom` / `headroom accounts`:** the board. Live limit bars for every
  account, refreshing itself while it is open; enter pins the account a bare
  `headroom launch` targets, and `a` turns automatic placement on instead
  (below). Under auto no row is `← current`: a header line says bare launches
  are automatic, and `← next` marks the account one would take from the
  figures on screen. With Codex present the board has a page per
  vendor: tab switches, each page keeps its own selection and refresh
  schedule, only the visible page fetches, and enter and `a` record that
  vendor's routing alone. An account with sessions busy on it or launches
  made in the last fifteen minutes gets a `sessions:` line — `2 busy, 1
  launched (steward-home: 1 busy)` names the share of another home that
  spends against the same ledger (below). Off a terminal it prints one
  frame — each vendor under a heading when there are two — and exits.
- **`headroom accounts --compact`:** the same board, one row per account: the
  email, then one cell per limit window in priority order (Claude Code:
  all models, 5h session, model-scoped 7d; Codex: the main limit, then code
  review, then additional limits, longer window first) — the percent in the
  bar's colour with the time to reset dim beside it (`52% 4.8d`; tenths of an
  hour under a day, `2.1h`; `not started` for a Codex window nobody has spent
  against) — then every warning the block would have shown (health, a vendor
  block, `stale`, provenance, drift, then the sessions and launches on it)
  at the end of the row. `●` marks the
  current account (`→` the next one, under auto), a red `!` (and a `dir says …` clause) a dir/login
  mismatch, a red name an account that cannot be used. On a narrow terminal
  headings shorten first, then the email; warnings lead the caption so the
  clip takes explanations before signals. Same keys, same refresh, same enter.
- **`headroom --json`:** the board as a versioned JSON document (schema 7) —
  probes and, budget permitting, fetches, for scripts that want a refresh.
  `accounts[]` is one flat list with a `vendor` on every account; `current`
  and `mode` are objects keyed by vendor. `mode` is `"pinned"`, `"auto"`, or
  `""` when `.current` cannot be resolved; under `"auto"` that vendor's
  `current` is `""` and no account carries `current: true`. Limits carry decoded identity (`kind`, `group`,
  `model`, and for Codex `feature`, `window_seconds`) and `unstarted`; `usage`
  carries `allowance` (`unknown` | `allowed` | `blocked` | `bad`) with
  `allowance_reason` and `blocked_features` when present. Every account
  carries `load`: `value` (what an automatic launch ranks by), `busy`,
  `launched` (launches of the last fifteen minutes not yet busy) and `homes`,
  one entry per home they came from (`home` its accounts root, `label`,
  `this` for the home the document was made in).
- **`headroom limits`:** what is already known, as the same JSON document,
  read from disk alone: no health probe, no network, never spends a request —
  ~10ms against the board's ~300ms, and no process sample either, so every
  account's `load` is `null`. `--account <name>` filters by name inside the
  selected vendors.
- **`headroom sessions`:** interactive picker over every Claude Code session
  on the machine; enter resumes in its own project dir on the account that
  last drove it, `x` resumes on the current account and re-homes it there —
  under auto, on the least-loaded of the other accounts
  (`--json` lists instead; `--cd-file <path>` writes the entered dir for the
  shell). Codex sessions are reached through Codex's own `resume`, below.
- **`headroom launch`:** exec `claude` — or `codex` under `--vendor codex` —
  on the decided account, with the child environment built from that
  decision: an inherited `CLAUDE_CONFIG_DIR`, or for Codex `CODEX_HOME`,
  `CODEX_SQLITE_HOME`, `CODEX_API_KEY` and `CODEX_ACCESS_TOKEN`, is stripped,
  never obeyed. Everything after `--` goes to the vendor's binary unchanged.
  How the launch is spelled decides the account:
  - bare — what the board recorded: the pinned account, or under auto the
    least-loaded one;
  - `--account <name>` — that account, whatever the mode;
  - `--auto` — this one launch placed automatically, the pin untouched;
  - `--last` — the account of the newest recorded launch.

  Every launch says on stderr which account it took and why, before the
  vendor starts: `<account> · auto · 5h 12% · week 33% · load 2 (next: …)`,
  `· pinned`, `· named`, `· last`; `load 3 (2 from steward-home)` says how
  much of it another home put there. A figure that is not a fresh measurement
  says so: `≥12%` is a lower bound from an observation older than fifteen
  minutes, `window ended` a window whose reset has passed, `?%` a percent
  that did not parse. A launch whose placement or log line could not be
  written still starts, and says so on a second line.
  `--remember` records the account for later bare launches, or with `--auto`
  the mode. `--dry-run` prints the choice and one row per account — figures,
  their age, busy sessions, pending launches, load, why an account was left
  out — and records, logs and starts nothing. Codex's sessions are shared
  across its accounts, so `headroom launch --vendor codex --account <other>
  -- resume` continues one on another account (`-- resume --all` lists every
  project's).
- **`headroom launches [-n <count>] [--json]`:** the newest launches from the
  log, one line each: when, which account, how it was decided, the load it was
  decided on and the runner-up. `--json` emits the records unchanged, one per
  line, with every account's figures as they were counted.
- **`headroom refresh`:** asks the usage endpoint about every account that may
  be asked, stores the answers and prints nothing — the round `--json` runs,
  without the health probes or the document. An automatic launch leaves one
  running in the background for the next launch: usually done in a second or
  two, never past about twelve seconds plus the write; SIGTERM, SIGINT or
  SIGHUP stop it within a second, with the request it abandoned recorded.
- **`headroom resolve [<name>]`:** prints the account's
  name/dir/kind for shell preflight. Under auto a name is required: there is
  no one account to print.
- **`headroom accounts add <email> [--share-config[=<dir>]]`:** seed the dir
  for a new subscription, its session store linked to the machine-global one
  (`projects/` for Claude Code, `sessions/` for Codex); `--share-config`
  symlinks a whitelist of the primary's config, or every entry of `<dir>` —
  never a login, history or session registry (`.credentials.json`,
  `.claude.json`, `history.jsonl`, `sessions/`), which stay per dir whatever
  the source holds and are named when skipped.
  Then log in once: Claude Code — `headroom launch --account <email>` and
  `/login`; Codex — `headroom launch --vendor codex --account <email> --
  login`.
- **`headroom accounts remove [<email>] [--yes]`:** bare, on a terminal, it
  offers a picker of the removable accounts; a name nobody answers to gets
  that list too. Confirms with `y/N`. Refuses while the account has a live
  session; deletes its Keychain item and its dir, scrubs `.order`, never
  touches `.current`. Also removes stranded `<name>.lock` debris. Codex
  accounts are never removed — headroom cannot tell whether a Codex session is
  running on a home — and the refusal names the directory to delete by hand.
- **`headroom accounts ledger [<accounts root>]`:** Claude Code only. With a
  root, this home spends against that root's ledger from now on (it writes
  `.ledger`; naming this home's own root stops sharing) — for a second home
  on the machine holding its own logins of the same subscriptions. Either
  way it registers this home in the ledger and lists the homes registered
  there.
- **`headroom check`:** verifies vendor contracts and headroom's own state,
  for every vendor present (run after a Claude Code or Codex update, or when
  the board looks wrong). Its `credential[...]` line names the Keychain or
  `.credentials.json` source and fails when no parseable login exists. It
  also fails when an account dir's login, history or session registry is a
  link, or when an account dir belongs to two homes sharing a ledger, and it
  says when this home's primary is spelled out and which homes share its
  ledger. Its `retention:` line prints how long the shared session store is
  kept and fails when accounts sharing it disagree on `cleanupPeriodDays`:
  every account's cleanup sweep prunes the one store, so the shortest wins. A configuration headroom refuses (a relative `HEADROOM_*`
  override, an unusable `.ledger`) exits 1 with a `config:` FAIL.
- **`headroom version`:** one line, `headroom <commit>[+dirty] <commit time>`
  — the commit the binary was built from. Answers whatever the environment
  or the files say.

## How it works

There is no account list: Claude Code's default `~/.claude` plus every
directory under `~/.claude-accounts` (one per extra login, named by its
email) *is* the account set — and `~/.codex` plus `~/.codex-accounts` is
Codex's, with its own `.current`, `.order` and `state.json`. Each Claude Code
account, including the primary, uses the Keychain item for its config dir
when readable; otherwise headroom reads `.credentials.json` inside the real
config dir. Headroom calls the same usage endpoint Claude Code's own `/usage`
screen calls and renders the result. That endpoint budgets roughly one
request per minute *per account*, so headroom records its requests and
responses. An early refresh replays headroom's newest answer instead of
showing an older one.
Bars and percentages keep their severity colours when observations become
stale; the stale caption and dim time fields communicate age separately.
The current response contract requires `limits[]`; historical usage envelopes
are reported as unreadable by `check`.

### Automatic placement

`a` on the board (or `headroom launch --auto --remember`) writes the word
`auto` into `.current` in place of an account name. From then on a bare launch
chooses for itself, and says what it chose:

```
headroom launch: alice@example.com · auto · 5h 1% · week 3% · load 0 (next: bob@example.com)
```

The rule gives each limit one job:

- **The five-hour window ranks accounts**, as *load*: one step per ten points
  of its usage, one per session the vendor reports as busy on that account,
  and one per launch made there in the last fifteen minutes. The least-loaded
  account wins.
- **Any limit at 80% or above sets an account aside** while another has room.
  When every account is near a limit, the one whose tightest limit is lowest
  takes the launch, and the line says so.
- **Weekly room breaks ties**, then the account launched least recently.

Usage figures come from disk: the launch itself asks no network, so it adds a
few tens of milliseconds. Figures older than fifteen minutes are *lower
bounds* — a window whose reset has passed counts as empty — and an account
that cannot be asked (its token aged out because nothing has used it) is tried
rather than avoided, with the line saying how old its figures are. Its own
launch then counts against it, so a second one does not follow blindly. An
account is left out only on positive evidence: not logged in, login expired,
blocked by the vendor, or a broken sessions link.

Choosing and recording happen in one locked step in `state.json`, so launches
started together — an agent fanning out several sessions — see each other.
Every launch is recorded, pinned and named ones included: they are load too.

A launch that names a session by id (`--resume <id>`, `--session-id <id>`)
follows the account that last drove it while that account is not near a limit,
so the session keeps its prompt cache; otherwise it is placed like a new one
and its new account is recorded, the way `x` in the picker records a move.
`--continue` and a bare `--resume` are placed like new sessions.

What placement does not do: it places once, at launch. A running session is
never moved, and a session started on an account can still exhaust it later.
The step, the 80% threshold and the fifteen minutes are policy, not
measurements.

Each launch appends one line to `launches.jsonl` beside `state.json`, with
every account's figures as counted. Nothing that routes reads that file:
deleting it changes no launch.

### A second home on the same machine

A second consumer of Claude Code under the same OS user — an automated
pipeline with its own `HOME`, say — can hold its own logins of the same
subscriptions in its own dirs and still share one picture of their load:

```sh
# as that home (HOME=/path/to/second-home)
headroom accounts ledger /Users/you/.claude-accounts    # spend against the first home's ledger
headroom accounts add alice@example.com --share-config  # its own dirs, its own primary's config

# as the first home, once
headroom accounts ledger                                # register it, so the second home can find its dirs
```

The ledger named must be a home's own: a root that itself spends against
another's ledger is refused. Membership is what each home's `.ledger` says
now — a home that leaves (naming its own root) stops counting at once, and
the listing marks its old registration `left — not counted`.

From then on a claim from either home is the claim the other is denied by —
each subscription is asked once per spacing, by whichever home asks first —
a figure either home bought is the one both place on, and every launch and
busy session on a subscription counts for both, whichever home it is in.
What a home owns stays its own: its dirs and logins, its `.current`, its
session store, its re-homes (in its own `state.json`), its launch log and
its `--last`. The first home's board names the other's share of each
account's load.

A home whose `HOME` is not the user's login home launches its primary with
`CLAUDE_CONFIG_DIR` set to its dir, never by the variable's absence, so it
reads its own `.claude.json` and never the login home's Keychain item.

Codex logins are plain files: each home's `auth.json` names the account, the
plan and the access token, and headroom reads the usage endpoint Codex's own
client reads. Before headroom's first fetch a Codex account's usage is
unknown — nothing is inferred from Codex's session files. When the vendor says
an account is blocked (a reached limit, spend control), the board says so
whatever the percentages read, and does not count it as a choice. Only Codex's
file credential store is read; a keyring login shows as not logged in, and
`check` says why.

On upgrade, legacy re-homes and outstanding cooldowns are imported into
`state.json` once, checkpointed before the inputs are archived as `.imported`
recovery copies. The longest imported cooldown briefly applies to all accounts,
bounded by 16 minutes. A damaged legacy request ledger waits that full interval;
unreadable legacy re-homes require repair before migration can commit.

`check` exits 0 for PASS, 1 for failed assertions, or 2 for INCONCLUSIVE.
Failures identify vendor drift or headroom's own state problems, and name the
vendor. Rate limiting, transport failure, lock contention, a valid newer state
schema and any Codex 401 are inconclusive. An unclaimed request is marked untested; a received response
can still be checked when recording it fails. The board likewise keeps its
endpoint result and figures visible alongside a persistence warning.

Session transcripts on this setup are machine-global (every account's
`projects/` links to one store), so `sessions` lists every conversation
regardless of account and routes each back to the account that last drove
it — changing the default steers new sessions; an old one moves to another account by `x` in the picker (or `launch --account … -- --resume <id>`).

headroom is **read-only** toward that system: it never refreshes a token and
no observation path writes anything of Claude Code's — Claude Code owns login
state. It keeps three files of its own (`state.json`, `.current` and
`launches.jsonl`), and a fourth, `.ledger`, when `accounts ledger` writes
it; the only
vendor-state mutations are explicit user commands naming their object — the
session picker's `rename`/`delete`, and `accounts remove` deleting the
removed account's own Keychain item — all refused while liveness is active
or unverifiable. Launch routing belongs to headroom too: it validates the
account and builds the child environment, neutralizing an inherited
`CLAUDE_CONFIG_DIR` (for Codex, `CODEX_HOME` and the ambient credential
variables). No Codex file is ever written. Shell wrappers provide personal
preflight and flags;
[DESIGN.md](../DESIGN.md) explains that ownership boundary and its vendor contracts.

## Shell integration

Short names are the shell's business; routing stays headroom's. The
patterns below are what the author runs, reduced to the engine calls:

```sh
# the two daily verbs
x()   { headroom launch -- "$@"; }                   # a session where the board says: the pinned account, or under auto the least-loaded
xa()  { headroom launch --account "$1" -- "${@:2}"; } # one session on <account>; the default stays
xacc(){ headroom accounts --compact; }               # the board, one row per account; enter pins, a turns auto on, then type x
xl()  { headroom launch --last -- "$@"; }            # one more session on the account the last launch used

# the session picker, with the cd that outlives the session
xs() {
  local tmp; tmp=$(mktemp -d) || return
  headroom sessions --cd-file "$tmp/cwd" -- "$@"     # enter: same account · x: current account + re-home
  local rc=$? dir; dir=$(cat "$tmp/cwd" 2>/dev/null); rm -rf "$tmp"
  [[ "$dir" == /* ]] && cd -- "$dir"
  return $rc
}

export HEADROOM_LAUNCHER_FORMAT="xa %s"              # the board advertises this spelling

# Codex: the same two verbs. Plain `codex` stays the vendor's own default.
cx()  { headroom launch --vendor codex -- "$@"; }
cxa() { headroom launch --vendor codex --account "$1" -- "${@:2}"; }   # cxa <account> resume → continue there
export HEADROOM_CODEX_LAUNCHER_FORMAT="cxa %s"
```

The out-of-quota flow in the README becomes `xacc`, then `xs` and `x` on the row
(under auto, just `xs` and `x`). A flag for headroom itself — `--auto`,
`--last`, `--dry-run` — needs a function of its own, because everything typed
after `x` goes to the vendor.
Flags every session should carry — `--dangerously-skip-permissions`, say —
go after the `--` inside the wrapper. Per-account names (`x-alice`) are a
loop over `~/.claude-accounts/*` at shell init; the author's version also
generates short local-part aliases when unambiguous.

A wrapper passes names and flags and nothing else. `CLAUDE_CONFIG_DIR`,
`CODEX_HOME`, the
current-account file and every validation stay in `headroom launch`, which
is re-resolved from PATH at every keystroke while a shell function is frozen
at shell init. When `headroom` is missing or refuses, a wrapper stops rather
than falling back to bare `claude` or `codex`.

A tool that spawns `claude` or `codex` headlessly joins the same door by
running `headroom launch [--vendor codex] --` in front of its arguments:
launch execs the binary, so stdio, signals and the exit status are the
child's. Under auto its sessions are placed like any other, and one that
passes `--session-id <uuid>` on its first turn and `--resume <uuid>` after
keeps one account across turns. The launch line goes to stderr. A tool that
must place automatically whatever the board has pinned spells `--auto`
(`headroom launch --auto --`); one running under a second home's `HOME`
needs nothing more once that home shares the ledger.

## Configuration

Nothing needs configuring on a standard install; environment variables re-point the defaults:

| Variable | Default | Meaning |
| --- | --- | --- |
| `HEADROOM_HOME` | `~` | Re-points everything home-derived, for both vendors: the primary dirs, the session stores, the accounts roots (test isolation) |
| `HEADROOM_ACCOUNTS_ROOT` | `~/.claude-accounts` | Where the extra account dirs live |
| `HEADROOM_LAUNCHER_FORMAT` | `headroom launch --account %s` | How the board spells the command that launches an account (`%s` = its name) — display only; a shell integration sets it to its own names (`x-%s`) |
| `HEADROOM_CODEX_ACCOUNTS_ROOT` | `~/.codex-accounts` | Where the extra Codex homes live. It must not name the same location as the Claude Code root |
| `HEADROOM_CODEX_LAUNCHER_FORMAT` | `headroom launch --vendor codex --account %s` | The Codex counterpart of `HEADROOM_LAUNCHER_FORMAT`. Log-in hints keep the engine's own spelling whatever this says |
| `HEADROOM_CODEX_PRIMARY_NAME` | *(derived)* | The name the primary `~/.codex` answers to, derived and pinned like `HEADROOM_PRIMARY_NAME` |
| `HEADROOM_PRIMARY_NAME` | *(derived)* | The name the primary `~/.claude` answers to — in `.current`, `--account`, and the board's launcher column. Unset, it is the local part of the primary's logged-in email (`alice` for `alice@example.com`; `primary` before any login). Set it to pin a name that must outlive a primary logout |

One setting is a file rather than a variable, because it belongs to a home
and must hold on every path that home's processes are started from:
`~/.claude-accounts/.ledger` names the accounts root whose ledger this home
spends against (written by `headroom accounts ledger`). Absent, the home has
its own; one that names no usable directory refuses every command.

## Developing

```bash
make check       # vet + tests (race detector on)
make test-pty    # the interactive surface, through a real pty (expect(1))
```

[CLAUDE.md](../CLAUDE.md) is the architecture; [DESIGN.md](../DESIGN.md) the
mental model and the reverse-engineered vendor contracts — read it before
touching parsers, `check`, the session store, or anything Keychain-related.
