# headroom

Several Claude Code subscriptions can coexist on one machine, each logged in
under its own config dir. headroom answers the question that setup creates —
**which account has quota left?** — in one glance:

```
alice@example.com (max 20x · headroom launch --account alice@example.com)
  5h session       [███░░░░░░░░░░░░░░░░░]  17%  resets 03:19 (in 2h 7m)
  All models (7d)  [████████████░░░░░░░░]  61%  resets Thu 17:59 (in 4d 16h)

bob@example.com (max 5x · headroom launch --account bob@example.com)  ← current
  5h session       [█████████░░░░░░░░░░░]  45%  resets 01:50 (in 37m)
  All models (7d)  [█░░░░░░░░░░░░░░░░░░░]   6%  resets Mon 17:00 (in 1d 15h)
```

## Install

```sh
go install github.com/qiushiyan/headroom/cmd/headroom@latest
# or from a clone: make install   (builds ~/.local/bin/headroom)
headroom check                    # PASS or INCONCLUSIVE on a fresh machine
```

Go and `golang.org/x/term` only — no cgo, no frameworks. macOS is the primary
target (credentials live in the Keychain); on machines without one, headroom
reads the `.credentials.json` Claude Code writes instead.

## Getting started

Your existing `~/.claude` login is the **primary**; each further subscription
becomes one directory under `~/.claude-accounts/`, named by its email:

```sh
headroom accounts add alice@example.com --share-config   # seed the dir (config shared from ~/.claude)
headroom launch --account alice@example.com              # claude opens on that account — /login there, once
headroom                                                 # the board
```

Repeat `add` per subscription; that is the setup. The `skills/` directory
ships two agent skills — `headroom` (reading the board, switching, resuming,
diagnosing) and `headroom-setup` (the steps above, plus folding in older
session dirs and shell launchers) — for any agent that runs the skills CLI:

```sh
npx skills add qiushiyan/headroom
```

## Commands

| Command | What it does |
| --- | --- |
| `headroom` / `headroom accounts` | The board: live limit bars for every account, refreshing itself while it is open; enter picks the account a bare `headroom launch` targets. Off a terminal, prints one frame and exits |
| `headroom --json` | The board as a versioned JSON document — probes and (budget permitting) fetches, for scripts that want a refresh |
| `headroom limits` | What is already known, as the same JSON document, read from disk alone (`--account <name>` scopes it): no health probe, no network, never spends a request — ~10ms against the board's ~300ms |
| `headroom sessions` | Interactive session picker: every session on the machine, resumed in its own project dir on the account that last drove it (`--json` lists instead) |
| `headroom launch` | Exec `claude` on the chosen account (`--account <name>`, or the recorded choice), with the child environment built from that decision — an inherited `CLAUDE_CONFIG_DIR` is stripped, never obeyed. `--remember` also records the choice; `headroom resolve` prints the account's name/dir/kind for shell preflight |
| `headroom accounts add <email> [--share-config[=<dir>]]` | Seed the dir for a new subscription: `projects/` linked to the machine-global session store; `--share-config` symlinks the primary's config (settings, skills, commands, hooks, …) or every entry of `<dir>`. Then `headroom launch --account <email>` and `/login` once |
| `headroom accounts remove <email> [--yes]` | Refuses while the account has a live session; deletes its Keychain item and its dir, scrubs `.order`, never touches `.current`. Also removes stranded `<name>.lock` debris |
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
it — quota switching steers new sessions, never old ones.

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
[DESIGN.md](DESIGN.md).

## Shell integration

Short names are the shell's business; routing stays headroom's. A wrapper
passes names and flags and nothing else — `CLAUDE_CONFIG_DIR`, the
current-account file and every validation stay in `headroom launch`, which
is re-resolved from PATH at every keystroke while a shell function is frozen
at shell init. `HEADROOM_LAUNCHER_FORMAT` tells the board how your shell
spells a launch. A copy-paste starter lives in
[`skills/headroom-setup/SKILL.md`](skills/headroom-setup/SKILL.md) § Shell
launchers.

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

[CLAUDE.md](CLAUDE.md) is the architecture; [DESIGN.md](DESIGN.md) the
mental model and the reverse-engineered vendor contracts — read it before
touching parsers, `check`, the session store, or anything Keychain-related.

## Status

Everything headroom assumes about Claude Code — the Keychain naming, the
usage endpoint and its response shape, the session store layout — is
reverse-engineered and perishable; `headroom check` exists precisely because
any Claude Code update may break it, and a FAIL there is the expected
failure mode, not a surprise. The usage endpoint is undocumented and the
vendor's consumer terms restrict it to first-party clients: headroom reads
it the way Claude Code's own `/usage` screen does, once a minute per account
at most, and never writes login or quota state. Install it as a read-only
instrument you understand, not as a supported product — there are no
releases; `go install` from source is the distribution.
