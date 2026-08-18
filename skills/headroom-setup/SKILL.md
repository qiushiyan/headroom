---
name: headroom-setup
description: Set up the headroom CLI for several Claude Code subscriptions on one machine — install it, add each subscription as an account and log in once, share config between accounts, keep session history machine-global, add or retire an account later, and optionally wire short shell launchers. Use when the user is installing headroom, has a second Claude subscription to add, wants to remove one, or is migrating existing per-account session directories.
---

The filesystem is the registry: `~/.claude` (Claude Code's default dir) is
the **primary**; every directory under `~/.claude-accounts/`, named by its
login email, is one **extra** account. Claude Code keys its credentials per
config dir, so each dir is an independent login and all coexist. headroom
makes the dirs; Claude Code does the logging in.

## Install

```sh
go install github.com/qiushiyan/headroom/cmd/headroom@latest   # or: git clone … && make install
headroom check
```

Done when `headroom check` prints PASS or INCONCLUSIVE — FAIL means a
Claude Code update changed something headroom relies on; stop and report
the FAIL line.

## Add a subscription

1. `headroom accounts add <email>` — the email the subscription will log
   in as. Adds `--share-config` when the user wants their settings, skills,
   commands, hooks and plugins from `~/.claude` in this account too (a
   whitelist; login state and history stay per account), or
   `--share-config=<dir>` to link every entry of a config package (a
   dotfiles dir).
2. Hand the user: `headroom launch --account <email>` then `/login` in
   that session, choosing **that** email.
3. `headroom` — the new row shows plan and bars. A red `(dir says …!)`
   means the wrong account was chosen at `/login`; `/login` again there.

Repeat per subscription. The primary needs nothing: its name on the board is
its login's local part (`alice` for `alice@example.com`); to pin a
different name, `export HEADROOM_PRIMARY_NAME=<name>` in the shell rc.
Board order after the primary is `~/.claude-accounts/.order` (one email
per line); unlisted accounts follow alphabetically.

## Sessions are machine-global

`accounts add` links each account's `projects/` to `~/.claude/projects`, so
`headroom sessions` from any account lists every conversation and resumes
each on the account that last drove it. Nothing to do for accounts seeded
this way.

An account dir that already existed with a *real* `projects/` directory
holds sessions only it can see; `headroom launch` refuses it on topology.
To fold them in (no `claude` running): move each `<dir>/projects/<project>/`
into `~/.claude/projects/<project>/` — same project names merge; on a
filename collision keep the newer file, the older is a stale copy — then
`rmdir <dir>/projects && ln -s ~/.claude/projects <dir>/projects`.
`headroom check` confirms the topology.

## Retire a subscription

`headroom accounts remove <email>` — refuses while that account has a live
session or an unmigrated `projects/`, asks for the dir name back (`--yes`
in scripts), deletes the account's own Keychain item and dir, scrubs
`.order`. Transcripts survive; if the default pointed here, `headroom
accounts` repicks. Also removes stranded `<name>.lock` debris.

## Shell launchers (optional)

Short names over `headroom launch` — the shell's convenience, headroom's
routing:

```sh
x()  { headroom launch -- "$@"; }                       # default account
xa() { headroom launch --account "$1" -- "${@:2}"; }    # one session on <account>
xs() { headroom sessions; }
export HEADROOM_LAUNCHER_FORMAT="xa %s"                 # board advertises this spelling
```

Wrappers pass flags and names only; `CLAUDE_CONFIG_DIR`, the current-account
file and every validation stay in `headroom launch`, which is re-resolved
from PATH at every keystroke while a shell function is frozen at shell
init.
