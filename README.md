# headroom

Several Claude Code subscriptions can coexist on one machine, each logged in
under its own config dir. headroom answers the question that setup creates —
**which account has quota left?** — in one glance:

```
alice@example.com (max 20x · x-alice)
  5h session       [███░░░░░░░░░░░░░░░░░]  17%  resets 03:19 (in 2h 7m)
  All models (7d)  [████████████░░░░░░░░]  61%  resets Thu 17:59 (in 4d 16h)

bob@example.com (max 5x · x-bob)  ← x
  5h session       [█████████░░░░░░░░░░░]  45%  resets 01:50 (in 37m)
  All models (7d)  [█░░░░░░░░░░░░░░░░░░░]   6%  resets Mon 17:00 (in 1d 15h)
```

## Commands

| Command | What it does |
| --- | --- |
| `headroom` / `headroom accounts` | The board: live limit bars for every account, refreshing itself while it is open; enter picks the account the shell's bare launcher targets. Off a terminal, prints one frame and exits |
| `headroom --json` | The same data as a versioned JSON document, for scripts and status lines |
| `headroom resume` | Interactive session picker: every session on the machine, resumed in its own project dir on the account that last drove it (`--json` lists instead) |
| `headroom launch` | Exec `claude` on the chosen account (`--account <name>`, or the recorded choice), with the child environment built from that decision — an inherited `CLAUDE_CONFIG_DIR` is stripped, never obeyed. `--remember` also records the choice; `headroom resolve` prints the account's name/dir/kind for shell preflight |
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
`projects/` links to one store), so `resume` lists every conversation
regardless of account and routes each back to the account that last drove
it — quota switching steers new sessions, never old ones.

headroom is **read-only** toward that system: it never writes the Keychain
and never refreshes a token — Claude Code owns both. It keeps two files of its
own (`state.json` and `.current`), and the session picker's explicit
`rename`/`delete` commands are the only two vendor-state mutations, both
refused while a session is open. Launch routing is headroom's too: `launch`
validates the account and constructs the child environment from that decision
alone, so a shell whose environment already carries a `CLAUDE_CONFIG_DIR`
(a tmux server started inside a Claude Code session, say) can never re-route
a launch. The `x-<name>` wrapper commands are shell integration — personal
preflight and flags over `headroom launch`; that split, and the
reverse-engineered vendor facts everything rests on, are spelled out in
[DESIGN.md](DESIGN.md).

## Building

```bash
make install   # builds ~/.local/bin/headroom
make check     # vet + tests (race detector on)
```

Go and `golang.org/x/term` only — no cgo, no frameworks.

## Configuration

Defaults fit the author's machine; environment variables re-point them:

| Variable | Default | Meaning |
| --- | --- | --- |
| `HEADROOM_HOME` | `~` | Re-points everything home-derived: the primary `~/.claude`, the session store, the accounts root (test isolation) |
| `HEADROOM_ACCOUNTS_ROOT` | `~/.claude-accounts` | Where the extra account dirs live |
| `HEADROOM_PRIMARY_NAME` | `qiushi` | Launcher name advertised for the primary `~/.claude` |

## Status

The usage endpoint headroom reads is undocumented, and the vendor's consumer
terms restrict it to first-party clients. This repository exists as a
personal, read-only instrument — source is public for reading, but there are
no releases and no packaging, and none are planned. Everything it assumes
about Claude Code is reverse-engineered and perishable; `headroom check`
exists precisely because any Claude Code update may break it.
