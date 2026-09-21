---
name: headroom
description: Day-to-day use of the headroom CLI — quota per Claude Code or Codex account, launching on another account, changing the default, continuing a rate-limited session on another account, checking the board after a vendor update.
disable-model-invocation: true
---

Several Claude Code logins coexist here, one per config dir: `~/.claude` is
the **primary**, `~/.claude-accounts/<email>` each **extra**. `headroom`
reads them all. Answer from its output, and hand every launcher to the user
as a command to run — a launcher execs `claude` in the user's terminal.

Names: an extra is its email; the primary is its login's local part
(`alice`), or `HEADROOM_PRIMARY_NAME`. The board's launcher column shows
the spelling this machine uses (`headroom launch --account <name>`, or a
shell's `x-<name>`) — quote that spelling.

## Read

- **Quota**: `headroom` — one frame off a terminal; every account's 5-hour
  and weekly bars, its launcher, `← current` on the default. `headroom
  limits` reads from disk and spends no request — the pick for a quick
  answer or a script; it always emits JSON. `headroom --json` refreshes
  and emits the board document.
- **Health lines, relayed as written**: *not logged in* / *login expired*
  → the printed launcher, then `/login`. *access token stale* → any session
  on that account refreshes it; only *login expired* calls for `/login`.
  Red `(dir says …!)` → `/login` in that dir chose the wrong account;
  `/login` there again.
- **Stale figures** keep their severity colours; their caption states the age.
- **Drift markers** → the vendor response changed shape; `headroom check`
  names what. A rolled-over window reads unknown until refreshed.

## Act

- **One session on another account**: `headroom launch --account <name>
  [-- <claude args>]`. The default stays where it was.
- **Change the default**: `headroom accounts`, enter on a row (records and
  exits); or `headroom launch --remember --account <name>`. `headroom
  accounts --compact` is the same board, one row per account: percent and
  time-to-reset per window, every warning at the row's end, same keys.
- **Out of quota mid-session — continue it on another account**. Changing
  the default steers new sessions only; the old one moves by re-home:
  1. Quit the session.
  2. `headroom accounts`, enter on an account with headroom left (skip if
     the default has some).
  3. `headroom sessions`, find the row, press **`x`** — continues on the
     current account, re-homed there. (Enter would return it to the
     exhausted account.)
  Without the picker: `headroom launch --account <name> -- --resume <id>`
  (`headroom sessions --json` lists ids), or `-- --continue` for the newest
  session in the cwd. Done when the session is running and its row shows
  the new owner.
- **Resume, same account**: `headroom sessions`, enter — every session on
  the machine, each continued in its own project dir on the account that
  last drove it.
- **After a Claude Code update, or a board that looks wrong**: `headroom
  check` — PASS / FAIL / INCONCLUSIVE. FAIL names the assumption that
  broke; INCONCLUSIVE (rate limited, stale token) is not drift.
- **Add or retire a subscription**: read `../headroom-setup/SKILL.md`
  (this file's sibling) and follow it.

## Codex

`~/.codex` is the Codex primary, `~/.codex-accounts/<email>` each extra, with
its own default. Everything above applies with `--vendor codex` on `launch`,
`resolve` and `accounts add`; the board, `headroom --json` and `headroom
limits` already show both vendors (`--vendor codex` for one), and every JSON
account carries `"vendor"`, with `current` keyed by vendor.

- **One session on another account**: `headroom launch --vendor codex
  --account <name> [-- <codex args>]`.
- **Change the default**: `headroom accounts`, **tab** to the Codex page,
  enter on a row.
- **Out of quota mid-session**: Codex sessions are shared across its
  accounts and `headroom sessions` does not list them — `headroom launch
  --vendor codex --account <other> -- resume` opens Codex's own picker for
  this directory (`-- resume --all` for every project).
- **Health lines**: *not logged in* → the printed command, which ends in
  `-- login` (Codex has no `/login`). *access token stale* / *access token
  rejected* → any Codex session on that account refreshes it. *blocked — …*
  → the vendor refuses work on that account whatever its percentages; pick
  another. *usage unknown* before headroom's first fetch is ordinary.
- **Retiring a Codex account** is by hand: `accounts remove --vendor codex`
  refuses and names the directory.

Routing lives in `headroom launch` — it owns `CLAUDE_CONFIG_DIR`,
`CODEX_HOME` and `.current`; the board's enter is what moves the default. The unmanaged
escape hatch, when the user asks for one, is `env -u CLAUDE_CONFIG_DIR
claude`. Flags beyond the ones above: `headroom -h`.
