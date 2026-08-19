# headroom reference

Commands, the model behind them, shell integration, configuration and the
developer loop. The [README](../README.md) is the user-facing story; this is
the rest. The mental model and the reverse-engineered vendor contracts are in
[DESIGN.md](../DESIGN.md); the architecture for contributors in
[CLAUDE.md](../CLAUDE.md).

## Commands

| Command | What it does |
| --- | --- |
| `headroom` / `headroom accounts` | The board: live limit bars for every account, refreshing itself while it is open; enter picks the account a bare `headroom launch` targets. Off a terminal, prints one frame and exits |
| `headroom --json` | The board as a versioned JSON document — probes and (budget permitting) fetches, for scripts that want a refresh |
| `headroom limits` | What is already known, as the same JSON document, read from disk alone (`--account <name>` scopes it): no health probe, no network, never spends a request — ~10ms against the board's ~300ms |
| `headroom sessions` | Interactive session picker: every session on the machine; enter resumes in its own project dir on the account that last drove it, `x` resumes on the current account and re-homes it there (`--json` lists instead; `--cd-file <path>` writes the entered dir for the shell) |
| `headroom launch` | Exec `claude` on the chosen account (`--account <name>`, or the recorded choice), with the child environment built from that decision — an inherited `CLAUDE_CONFIG_DIR` is stripped, never obeyed. `--remember` also records the choice; `headroom resolve` prints the account's name/dir/kind for shell preflight |
| `headroom accounts add <email> [--share-config[=<dir>]]` | Seed the dir for a new subscription: `projects/` linked to the machine-global session store; `--share-config` symlinks the primary's config (settings, skills, commands, hooks, …) or every entry of `<dir>`. Then `headroom launch --account <email>` and `/login` once |
| `headroom accounts remove [<email>] [--yes]` | Bare, on a terminal, it offers a picker of the removable accounts; a name nobody answers to gets that list too. Confirms with `y/N`. Refuses while the account has a live session; deletes its Keychain item and its dir, scrubs `.order`, never touches `.current`. Also removes stranded `<name>.lock` debris |
| `headroom check` | Verifies the reverse-engineered assumptions still hold (run after a Claude Code update) |

## How it works

There is no account list: Claude Code's default `~/.claude` plus every
directory under `~/.claude-accounts` (one per extra login, named by its
email) *is* the account set. Claude Code keys its macOS Keychain credentials
per config dir, so every dir is an independent login and all tokens coexist.
headroom reads each account's credentials, calls the same usage endpoint
Claude Code's own `/usage` screen calls, and renders the result. That endpoint
budgets roughly one request per minute *per account*, so headroom keeps a
record of both what it asked and what came back: a refresh that is too soon to
send replays its own newest answer instead of showing you something older.

Session transcripts on this setup are machine-global (every account's
`projects/` links to one store), so `sessions` lists every conversation
regardless of account and routes each back to the account that last drove
it — changing the default steers new sessions; an old one moves to another account by `x` in the picker (or `launch --account … -- --resume <id>`).

headroom is **read-only** toward that system: it never refreshes a token and
no observation path writes anything of Claude Code's — Claude Code owns login
state. It keeps two files of its own (`state.json` and `.current`); the only
vendor-state mutations are explicit user commands naming their object — the
session picker's `rename`/`delete`, and `accounts remove` deleting the
removed account's own Keychain item — all refused while a session is open. Launch routing is headroom's too: `launch`
validates the account and constructs the child environment from that decision
alone, so a shell whose environment already carries a `CLAUDE_CONFIG_DIR`
(a tmux server started inside a Claude Code session, say) can never re-route
a launch. The `x-<name>` wrapper commands are shell integration — personal
preflight and flags over `headroom launch`; that split, and the
reverse-engineered vendor facts everything rests on, are spelled out in
[DESIGN.md](../DESIGN.md).

## Shell integration

Short names are the shell's business; routing stays headroom's. The
patterns below are what the author runs, reduced to the engine calls:

```sh
# the two daily verbs
x()   { headroom launch -- "$@"; }                   # a session on the default account
xa()  { headroom launch --account "$1" -- "${@:2}"; } # one session on <account>; the default stays
xacc(){ headroom accounts; }                         # the board; enter moves the default, then type x

# the session picker, with the cd that outlives the session
xs() {
  local tmp; tmp=$(mktemp -d) || return
  headroom sessions --cd-file "$tmp/cwd" -- "$@"     # enter: same account · x: current account + re-home
  local rc=$? dir; dir=$(cat "$tmp/cwd" 2>/dev/null); rm -rf "$tmp"
  [[ "$dir" == /* ]] && cd -- "$dir"
  return $rc
}

export HEADROOM_LAUNCHER_FORMAT="xa %s"              # the board advertises this spelling
```

The out-of-quota flow in the README becomes `xacc`, then `xs` and `x` on the row.
Flags every session should carry — `--dangerously-skip-permissions`, say —
go after the `--` inside the wrapper. Per-account names (`x-alice`) are a
loop over `~/.claude-accounts/*` at shell init; the author's version also
generates short local-part aliases when unambiguous.

A wrapper passes names and flags and nothing else. `CLAUDE_CONFIG_DIR`, the
current-account file and every validation stay in `headroom launch`, which
is re-resolved from PATH at every keystroke while a shell function is frozen
at shell init. When `headroom` is missing or refuses, a wrapper stops rather
than falling back to bare `claude`.

## Configuration

Nothing needs configuring on a standard install; environment variables re-point the defaults:

| Variable | Default | Meaning |
| --- | --- | --- |
| `HEADROOM_HOME` | `~` | Re-points everything home-derived: the primary `~/.claude`, the session store, the accounts root (test isolation) |
| `HEADROOM_ACCOUNTS_ROOT` | `~/.claude-accounts` | Where the extra account dirs live |
| `HEADROOM_LAUNCHER_FORMAT` | `headroom launch --account %s` | How the board spells the command that launches an account (`%s` = its name) — display only; a shell integration sets it to its own names (`x-%s`) |
| `HEADROOM_PRIMARY_NAME` | *(derived)* | The name the primary `~/.claude` answers to — in `.current`, `--account`, and the board's launcher column. Unset, it is the local part of the primary's logged-in email (`alice` for `alice@example.com`; `primary` before any login). Set it to pin a name that must outlive a primary logout |

## Developing

```bash
make check       # vet + tests (race detector on)
make test-pty    # the interactive surface, through a real pty (expect(1))
```

[CLAUDE.md](../CLAUDE.md) is the architecture; [DESIGN.md](../DESIGN.md) the
mental model and the reverse-engineered vendor contracts — read it before
touching parsers, `check`, the session store, or anything Keychain-related.
