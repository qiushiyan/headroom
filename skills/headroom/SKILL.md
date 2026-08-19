---
name: headroom
description: Day-to-day use of the headroom CLI — quota per Claude Code account, launching on another account, changing the default, continuing a rate-limited session on another account, checking the board after a Claude Code update.
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
  answer or a script (`--json` on either for a document).
- **Health lines, relayed as written**: *not logged in* / *login expired*
  → the printed launcher, then `/login`. *access token stale* → any session
  on that account refreshes it; only *login expired* calls for `/login`.
  Red `(dir says …!)` → `/login` in that dir chose the wrong account;
  `/login` there again.
- **`?` bars and drift markers** → the vendor response changed shape;
  `headroom check` names what.

## Act

- **One session on another account**: `headroom launch --account <name>
  [-- <claude args>]`. The default stays where it was.
- **Change the default**: `headroom accounts`, enter on a row (records and
  exits); or `headroom launch --remember --account <name>`.
- **Out of quota mid-session — continue on another account**: quit the
  session; `headroom accounts`, enter on an account with headroom left
  (skip if the default has some); `headroom sessions`, find the row, press
  **`x`** — it continues on the current account and is re-homed there.
  Enter would return it to the exhausted account. Without the picker:
  `headroom launch --account <name> -- --resume <id>` (`headroom sessions
  --json` lists ids), or `-- --continue` for the newest session in the cwd.
- **Resume, same account**: `headroom sessions`, enter — every session on
  the machine, each continued in its own project dir on the account that
  last drove it. Changing the default steers new sessions only.
- **After a Claude Code update, or a board that looks wrong**: `headroom
  check` — PASS / FAIL / INCONCLUSIVE. FAIL names the assumption that
  broke; INCONCLUSIVE (rate limited, stale token) is not drift.
- **Add or retire a subscription**: read `../headroom-setup/SKILL.md`
  (this file's sibling) and follow it.

Routing lives in `headroom launch` — it owns `CLAUDE_CONFIG_DIR` and
`.current`; the board's enter is what moves the default. The unmanaged
escape hatch, when the user asks for one, is `env -u CLAUDE_CONFIG_DIR
claude`. Flags beyond the ones above: `headroom -h`.
