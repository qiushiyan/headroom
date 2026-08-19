# headroom

Several Claude Code subscriptions on one machine, each logged in under its
own config dir. headroom is the instrument for that setup: it shows which
account has quota left, starts sessions on the account you choose, and lets
a conversation move between accounts — so a rate limit on one subscription
stops nothing.

```
alice@example.com (max 20x · headroom launch --account alice@example.com)
  5h session       [███░░░░░░░░░░░░░░░░░]  17%  resets 03:19 (in 2h 7m)
  All models (7d)  [████████████░░░░░░░░]  61%  resets Thu 17:59 (in 4d 16h)

bob@example.com (max 5x · headroom launch --account bob@example.com)  ← current
  5h session       [█████████░░░░░░░░░░░]  45%  resets 01:50 (in 37m)
  All models (7d)  [█░░░░░░░░░░░░░░░░░░░]   6%  resets Mon 17:00 (in 1d 15h)
```

## Use cases

### 1. See how much each account has left

`headroom` is the board: every account's 5-hour and weekly limits as live
bars, refreshing while it stays open, with the command that launches each
account and `← current` on the one a bare launch targets. It reads the same
endpoint Claude Code's own `/usage` screen reads — once a minute per account
at most — and replays its last answer when a refresh would be too soon, so
the number you see is never older than what it could have fetched.

`headroom limits` answers from disk with no network at all, for scripts and
status lines; `--json` on either gives the board as a document.

### 2. Spread sessions across accounts

Each subscription has its own 5-hour window. Start work on the account with
room and leave the others to recover:

- `headroom accounts` — pick where new sessions go. Enter records it and
  exits; from then on a bare `headroom launch` starts `claude` there.
- `headroom launch --account <email>` — one session on another account, the
  default untouched. Anything after `--` goes to `claude`.

Every session, whichever account started it, shows up in one picker:
`headroom sessions` lists every conversation on the machine and resumes each
in its own project directory, on the account that last drove it.

### 3. Out of quota mid-session? Continue on another account

A session on account A hits its limit with work unfinished. The transcript
belongs to the machine, not the account, so any account can pick it up:

1. Quit the session.
2. `headroom accounts` — enter on an account with headroom left (skip if
   the default already has some).
3. `headroom sessions` — find the row, press **`x`**: the session continues
   on the current account, in its own project dir, and is re-homed there.
   (Enter would send it back to A.)

Without the picker: `headroom launch --account <email> -- --resume <id>`
(`headroom sessions --json` lists ids), or `-- --continue` for the newest
session in the current directory. Ownership follows automatically — the
account that drives a session last is its owner.

## Install

```sh
go install github.com/qiushiyan/headroom/cmd/headroom@latest
headroom check      # PASS or INCONCLUSIVE on a fresh machine
```

macOS is the primary target (Claude Code keeps its credentials in the
Keychain); on machines without one, headroom reads the `.credentials.json`
Claude Code writes instead. Go and `golang.org/x/term` only.

## Set up your accounts

Your existing `~/.claude` login is the **primary**. Each further subscription
becomes one directory under `~/.claude-accounts/`, named by its email:

```sh
headroom accounts add alice@example.com --share-config   # seed the dir; share settings/skills/hooks from ~/.claude
headroom launch --account alice@example.com              # claude opens on that account — /login there, once
headroom                                                 # the new row is on the board
```

Repeat per subscription; that is the setup. `--share-config` is optional
(login state and history always stay per account). To retire one:
`headroom accounts remove <email>`.

**Shell shortcuts** (`x`, `xa`, `xs`, …) and the full command reference live
in [docs/REFERENCE.md](docs/REFERENCE.md).

## For your agent

The repo ships two agent skills — `headroom` (reading the board, switching,
the out-of-quota flow, diagnosing) and `headroom-setup` (the setup above,
plus folding in older session dirs and shell launchers):

```sh
npx skills add qiushiyan/headroom
```

## Status

Everything headroom assumes about Claude Code — Keychain naming, the usage
endpoint and its response shape, the session store layout — is
reverse-engineered and perishable. `headroom check` exists precisely because
any Claude Code update may break it; run it when something looks wrong. The
usage endpoint is undocumented and the vendor's consumer terms restrict it to
first-party clients: headroom reads it the way Claude Code's own `/usage`
screen does and never writes login or quota state. Install it as a read-only
instrument you understand, not as a supported product — there are no
releases; `go install` from source is the distribution.

How it works, every command, configuration, developing:
[docs/REFERENCE.md](docs/REFERENCE.md) · design and vendor contracts:
[DESIGN.md](DESIGN.md).
