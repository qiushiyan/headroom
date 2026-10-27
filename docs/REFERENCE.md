# headroom reference

Commands, the model behind them, shell integration, configuration and the
developer loop. The [README](../README.md) is the user-facing story; this is
the rest. The mental model, the reverse-engineered vendor contracts and the
code's ownership map are in [DESIGN.md](../DESIGN.md); what a change must
keep, for contributors, in [AGENTS.md](../AGENTS.md).

## Commands

`headroom --help` lists every command, and `headroom <command> --help` is that
command's reference: what it does, its flags with their defaults and allowed
values, what it prints, and its exit codes. The pages are generated from the
grammar the binary parses, and print whatever the configuration says. What
holds across commands:

- **Which vendor.** Commands that act on one account — `launch`, `resolve`,
  `accounts add`, `accounts remove` — act on Claude Code unless `--vendor
  codex` says otherwise, so an invocation without it means what it always
  meant. Commands that report — the board, `limits`, `launches`, `refresh` —
  cover every vendor on the machine unless `--vendor` names one. `login`,
  `sessions` and `headless` are Claude Code only. A command naming a vendor
  this machine does not have fails saying so, except `accounts add`, which is
  how the first Codex home comes to exist.
- **Arguments for the vendor.** `launch` and `sessions` pass their arguments to
  `claude` (or `codex`) unchanged. headroom's flags come first and end at the
  first argument or at `--`: `headroom launch fix it` and `headroom launch --
  --resume <id>` both reach the vendor, and a vendor flag before any argument
  needs the `--`.
- **Errors and exit codes.** A failure prints `headroom: error: <what failed>`
  on stderr and exits 1. Arguments headroom cannot accept also print the
  command's usage line and exit 2, having run nothing. `check` exits 2 for
  INCONCLUSIVE, `headless place` exits 3 when every account it could take
  is refused, and `login start`, `finish` and `cancel` exit 3 when they
  refused with nothing spent or sent — each printing its JSON document for
  every outcome that names an account. A configuration headroom refuses — a relative `HEADROOM_*`
  override, an unusable `.ledger` — makes every command except help,
  `version`, `check` and the retired `resume` exit 1 until it is fixed;
  `check` reports it as a `config:` FAIL.
- **The board, three ways.** `headroom`, `headroom board` and `headroom
  accounts` are the same board; `--compact` draws one row per account and
  `--json` prints the document after one refresh round. `headroom limits`
  prints that document from disk alone, for a script or a status line.
- **The retired `resume`** answers with how to fix the stale shell that called
  it, on stderr, and exits 2: its stdout was a protocol a stale shell
  function misread, so the name never comes back with another meaning.

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
headroom launch: alice@example.com · auto · 5h 1% · week 3%, resets in 4.2d · load 0 (next: bob@example.com)
```

The rule gives each limit one job:

- **The five-hour window ranks accounts**, as *load*: one step per ten points
  of its usage, one per session the vendor reports as busy on that account,
  and one per launch made there in the last fifteen minutes. The least-loaded
  account wins.
- **Any limit at 80% or above sets an account aside** while another has room.
  When every account is near a limit, the one whose tightest limit is lowest
  takes the launch, and the line says so.
- **Weekly room that lapses first breaks ties**: what is unspent at a reset
  is gone, so among equally loaded accounts the most room below 80% per hour
  its window has left goes first, judged on each account's tightest weekly
  limit. Then the account launched least recently.

Usage figures come from disk: the launch itself asks no network, so it adds a
few tens of milliseconds. Figures older than fifteen minutes are *lower
bounds* — a window whose reset has passed counts as empty, and a Claude Code
weekly window as renewing seven days after the reset that passed — and an account
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
everything the choice was made from, and every usage reading headroom
fetches appends one to `usage.jsonl` beside the ledger's `state.json`.
`headroom launches --eval` joins the two. Nothing that routes reads either
file: deleting them changes no launch.

### Renewing a login from elsewhere

`headroom login` renews at this machine's terminal. An approver elsewhere —
an agent with shell access, or you on another machine — renews one account
in commands that each return, and nothing opens here:

```sh
headroom login --dry-run --json                  # the plan: each login's end, what is due, what is held
headroom login start alice@example.com           # JSON with the URL; a holder waits ten minutes for the code
printf '%s\n' 'code#state' | headroom login finish alice@example.com   # sends it once; renewed, or why not
headroom login status alice@example.com          # the sign-in as it stands, and how it ended
```

Exit 3 means this call spent or sent nothing — the document's `reason` says
what to fix, and the same command runs again, except on `exchanging`, when a
code already went: ask `status`. Exit 1 means the sign-in is over, except on
`no_reply`, when the answer was lost and the code may have gone: ask
`status`, never `start`. One
sign-in per account: a second `start` returns the same URL, and the batch
and a sign-in never renew one account at once. An account whose login lives
in a Keychain the session cannot read — an ssh session's, or a locked one —
is refused at `start`, before any approval. `headroom login start --help`
and `finish --help` list every reason;
[docs/specs/login-handoff.md](specs/login-handoff.md) is the contract.

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
account's load, and both homes' readings land in the first home's usage log.

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
state. It keeps files of its own — `state.json`, `.current`, the launch log
`launches.jsonl`, the usage log `usage.jsonl`, and `.login/` for each
account's login lock and handed-off sign-in — and `.ledger` when
`accounts ledger` writes it; the only
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
make check       # vet, go fix -diff, the race-enabled suite, gofmt
make test-pty    # the interactive surface, through a real pty (expect(1))
make golden      # rewrite the help pages after an intended change; review the diff
make install     # build into ~/.local/bin, renamed into place
```

[AGENTS.md](../AGENTS.md) is what a change must keep;
[DESIGN.md](../DESIGN.md) the mental model, the reverse-engineered vendor
contracts and which package owns what — read it before touching parsers,
`check`, the session store, or anything Keychain-related.
