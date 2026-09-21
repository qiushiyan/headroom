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
board, `--json`, `limits` — show every vendor present on the machine and take
`--vendor` to show one. `check` and `sessions` take no `--vendor`. A command
naming a vendor this machine does not have fails saying so, except
`accounts add`.

- **`headroom` / `headroom accounts`:** the board. Live limit bars for every
  account, refreshing itself while it is open; enter picks the account a bare
  `headroom launch` targets. With Codex present the board has a page per
  vendor: tab switches, each page keeps its own selection and refresh
  schedule, only the visible page fetches, and enter records that vendor's
  account alone. Off a terminal it prints one frame — each vendor under a
  heading when there are two — and exits.
- **`headroom accounts --compact`:** the same board, one row per account: the
  email, then one cell per limit window in priority order (Claude Code:
  model-scoped 7d, 5h session, all models; Codex: the main limit, then code
  review, then additional limits, longer window first) — the percent in the
  bar's colour with the time to reset dim beside it (`52% 4.8d`; tenths of an
  hour under a day, `2.1h`; `not started` for a Codex window nobody has spent
  against) — then every warning the block would have shown (health, a vendor
  block, `stale`, provenance, drift) at the end of the row. `●` marks the
  current account, a red `!` (and a `dir says …` clause) a dir/login
  mismatch, a red name an account that cannot be used. On a narrow terminal
  headings shorten first, then the email; warnings lead the caption so the
  clip takes explanations before signals. Same keys, same refresh, same enter.
- **`headroom --json`:** the board as a versioned JSON document (schema 5) —
  probes and, budget permitting, fetches, for scripts that want a refresh.
  `accounts[]` is one flat list with a `vendor` on every account; `current` is
  an object keyed by vendor. Limits carry decoded identity (`kind`, `group`,
  `model`, and for Codex `feature`, `window_seconds`) and `unstarted`; `usage`
  carries `allowance` (`unknown` | `allowed` | `blocked` | `bad`) with
  `allowance_reason` and `blocked_features` when present.
- **`headroom limits`:** what is already known, as the same JSON document,
  read from disk alone: no health probe, no network, never spends a request —
  ~10ms against the board's ~300ms. `--account <name>` filters by name inside
  the selected vendors.
- **`headroom sessions`:** interactive picker over every Claude Code session
  on the machine; enter resumes in its own project dir on the account that
  last drove it, `x` resumes on the current account and re-homes it there
  (`--json` lists instead; `--cd-file <path>` writes the entered dir for the
  shell). Codex sessions are reached through Codex's own `resume`, below.
- **`headroom launch`:** exec `claude` — or `codex` under `--vendor codex` —
  on the chosen account (`--account <name>`, or the recorded choice), with the
  child environment built from that decision: an inherited
  `CLAUDE_CONFIG_DIR`, or for Codex `CODEX_HOME`, `CODEX_SQLITE_HOME`,
  `CODEX_API_KEY` and `CODEX_ACCESS_TOKEN`, is stripped, never obeyed.
  `--remember` also records the choice. Everything after `--` goes to the
  vendor's binary: Codex's sessions are shared across its accounts, so
  `headroom launch --vendor codex --account <other> -- resume` continues one
  on another account (`-- resume --all` lists every project's).
- **`headroom resolve [<name>]`:** prints the account's
  name/dir/kind for shell preflight.
- **`headroom accounts add <email> [--share-config[=<dir>]]`:** seed the dir
  for a new subscription, its session store linked to the machine-global one
  (`projects/` for Claude Code, `sessions/` for Codex); `--share-config`
  symlinks a whitelist of the primary's config, or every entry of `<dir>`.
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
- **`headroom check`:** verifies vendor contracts and headroom's own state,
  for every vendor present (run after a Claude Code or Codex update, or when
  the board looks wrong).

## How it works

There is no account list: Claude Code's default `~/.claude` plus every
directory under `~/.claude-accounts` (one per extra login, named by its
email) *is* the account set — and `~/.codex` plus `~/.codex-accounts` is
Codex's, with its own `.current`, `.order` and `state.json`. Claude Code keys its macOS Keychain credentials
per config dir, so every dir is an independent login and all tokens coexist.
headroom reads each account's credentials, calls the same usage endpoint
Claude Code's own `/usage` screen calls, and renders the result. That endpoint
budgets roughly one request per minute *per account*, so headroom keeps a
record of both what it asked and what came back: a refresh that is too soon to
send replays its own newest answer instead of showing you something older.
Bars and percentages keep their severity colours when observations become
stale; the stale caption and dim time fields communicate age separately.
The current response contract requires `limits[]`; historical usage envelopes
are reported as unreadable by `check`.

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
state. It keeps two files of its own (`state.json` and `.current`); the only
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
x()   { headroom launch -- "$@"; }                   # a session on the default account
xa()  { headroom launch --account "$1" -- "${@:2}"; } # one session on <account>; the default stays
xacc(){ headroom accounts --compact; }               # the board, one row per account; enter moves the default, then type x

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

The out-of-quota flow in the README becomes `xacc`, then `xs` and `x` on the row.
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
child's.

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

## Developing

```bash
make check       # vet + tests (race detector on)
make test-pty    # the interactive surface, through a real pty (expect(1))
```

[CLAUDE.md](../CLAUDE.md) is the architecture; [DESIGN.md](../DESIGN.md) the
mental model and the reverse-engineered vendor contracts — read it before
touching parsers, `check`, the session store, or anything Keychain-related.
