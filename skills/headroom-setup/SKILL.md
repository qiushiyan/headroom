---
name: headroom-setup
description: Set up headroom for several Claude Code subscriptions — install, add and log in each account, share config, keep sessions machine-global, retire an account, shell launchers.
disable-model-invocation: true
---

The filesystem is the registry: `~/.claude` (Claude Code's default dir) is
the **primary**; each directory under `~/.claude-accounts/`, named by its
login email, is one **extra**. Claude Code keys credentials per config dir,
so the logins coexist. headroom makes the dirs and routes launches; Claude
Code does the logging in.

## Install

```sh
go install github.com/qiushiyan/headroom/cmd/headroom@latest   # or from a clone: make install
headroom check
```

Done when `check` prints PASS or INCONCLUSIVE. FAIL means a Claude Code
update changed something headroom relies on — report the FAIL line and stop.

## Add a subscription

1. `headroom accounts add <email>` — the email it will log in as. Add
   `--share-config` when the user wants their `~/.claude` settings, skills,
   commands, hooks and plugins in this account too (a whitelist; login state
   and history stay per account); `--share-config=<dir>` links every entry
   of a config package instead.
2. Hand the user: `headroom launch --account <email>`, then `/login` in that
   session choosing **that** email.
3. `headroom`.

Done when the new row shows a plan and bars with no red `(dir says …!)` —
that warning means the wrong account was chosen at `/login`; `/login` there
again. Repeat per subscription. The primary needs nothing; its board name is
its login's local part (`alice`), or `export HEADROOM_PRIMARY_NAME=<name>`.
Board order after the primary: `~/.claude-accounts/.order`, one email per
line.

## Sessions are machine-global

`accounts add` links each account's `projects/` to `~/.claude/projects`, so
`headroom sessions` from any account lists every conversation and resumes
each on the account that last drove it. Accounts seeded this way are done.

An account dir that predates headroom with a *real* `projects/` directory
holds sessions only it can see, and `headroom launch` refuses it. Fold them
in with no `claude` running: move each `<dir>/projects/<project>/` into
`~/.claude/projects/<project>/` (same names merge; on a filename collision
keep the newer file), then `rmdir <dir>/projects && ln -s ~/.claude/projects
<dir>/projects`. Done when `headroom check` passes topology and
`headroom launch --account <email>` starts.

## Retire a subscription

`headroom accounts remove <email>` — it refuses while that account has a
live session or an unmigrated `projects/`, asks for the dir name back
(`--yes` in scripts), deletes the account's own Keychain item and dir, and
scrubs `.order`. Transcripts survive. Done when `headroom` no longer lists
the account; if the default pointed there, `headroom accounts` repicks.
The same command clears stranded `<name>.lock` debris.

## Shell launchers (optional)

Short names over the engine — the shell owns the spelling, headroom owns
the routing:

```sh
x()  { headroom launch -- "$@"; }                       # default account
xa() { headroom launch --account "$1" -- "${@:2}"; }    # one session on <account>
xs() { headroom sessions; }
export HEADROOM_LAUNCHER_FORMAT="xa %s"                 # the board advertises this spelling
```

A wrapper passes names and flags and nothing else: `CLAUDE_CONFIG_DIR`,
`.current` and every check stay in `headroom launch`, re-resolved from
PATH at each keystroke, while a shell function is frozen at shell init.
