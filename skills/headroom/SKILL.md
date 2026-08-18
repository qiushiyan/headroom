---
name: headroom
description: Multiple Claude Code accounts on one machine via the headroom CLI — which account has quota left, launching a session on a specific account, changing the default account, resuming a session across accounts, and diagnosing the board after a Claude Code update. Use when the user asks about Claude usage limits or "which account should I use", wants this session on another account, hits a rate limit, or headroom prints something unexpected.
---

Several Claude Code logins coexist on this machine, one per config dir
(`~/.claude` is the primary; `~/.claude-accounts/<email>` each extra). The
`headroom` CLI reads them all. Answer from its output; hand interactive
steps to the user as commands to run — launchers exec `claude` in *their*
terminal, so an agent never runs one itself.

## Read

- **Which account has quota left**: `headroom` (off a terminal it prints
  one frame and exits) — every account's 5-hour and weekly bars, the command
  that launches it, and `← current` on the account bare launches target. `headroom
  --json` for the same as a document. `headroom limits` answers from disk
  alone and never spends a request: prefer it for a quick read or a script;
  `--account <name>` scopes it.
- **Health lines are precise — relay them as written**: *not logged in* /
  *login expired* → the printed launcher and `/login`; *access token stale*
  → any session on that account refreshes it, `/login` is not needed. A
  red `(dir says …!)` means `/login` picked the wrong account in that dir —
  fix by `/login` again there.
- **Rows without numbers** (`?` bars, drift markers) mean the vendor
  response changed shape, not free headroom: run `headroom check`.

## Act

- **One session on another account**: `headroom launch --account <name>
  [-- <claude args>]` — the default account is untouched. `<name>` is the
  email (the primary answers to its login's local part, or whatever
  `HEADROOM_PRIMARY_NAME` pins). The board's launcher column shows the
  spelling *this* machine uses (a shell may wrap it as `x-<name>`); quote
  that spelling to the user.
- **Change the default**: `headroom accounts` — enter on a row records it
  and exits; the next `headroom launch` (with no `--account`) goes there.
  Non-interactive: `headroom launch --remember --account <name>`.
- **Resume a session, whichever account made it**: `headroom sessions` —
  every session on the machine, continued in its own project dir on the
  account that last drove it; `--json` lists without a terminal. Quota
  switching steers new sessions only.
- **After a Claude Code update, or when the board misbehaves**: `headroom
  check` — PASS / FAIL / INCONCLUSIVE (exit 0/1/2). A FAIL names the
  reverse-engineered assumption that broke; INCONCLUSIVE (rate limited,
  stale token) is not drift.
- **Add or retire a subscription**: the `headroom-setup` skill.

## Hands off

`CLAUDE_CONFIG_DIR`, `~/.claude-accounts/.current` and `state.json` are
headroom's routing state — launches route through `headroom launch`, and
the board's enter is what moves `.current`. Setting the variable by hand
or editing those files bypasses the validation the tool exists for; the
unmanaged escape hatch is `env -u CLAUDE_CONFIG_DIR claude`.
