---
name: headroom-setup
description: Set up headroom for several Claude Code or Codex subscriptions — install, add and log in each account, share config, keep sessions machine-global, retire an account, shell launchers.
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

Done when `check` prints PASS or INCONCLUSIVE. FAIL identifies either a
vendor-contract change or a problem in headroom’s own files; report the
named failure.

## Add a subscription

1. `headroom accounts add <email>` — the email it will log in as. Add
   `--share-config` when the user wants their `~/.claude` settings, skills,
   commands, hooks and plugins in this account too (a whitelist; login state
   and history stay per account); `--share-config=<dir>` links every entry
   of a config package instead.
2. Hand the user: `headroom launch --account <email>`, then `/login` in that
   session choosing **that** email.
3. `headroom` — the board.

Done when the new row shows a plan and bars with no red `(dir says …!)` —
that warning means the wrong account was chosen at `/login`; `/login` there
again. Repeat per subscription. The primary needs nothing; its board name is
its login's local part (`alice`), or `export HEADROOM_PRIMARY_NAME=<name>`.
Board order after the primary: `~/.claude-accounts/.order`, one email per
line.

## Add a Codex subscription

`~/.codex` is the Codex primary; each extra is a home under
`~/.codex-accounts/`, named by its login email.

1. `headroom accounts add --vendor codex <email>` — works before any Codex
   directory exists. `--share-config` links `config.toml`, `AGENTS.md`,
   themes, skills, prompts, rules and plugins from `~/.codex` (a whitelist:
   `auth.json`, history and the session index stay per home);
   `--share-config=<dir>` links every entry of a config package.
2. Hand the user: `headroom launch --vendor codex --account <email> -- login`,
   logging in as **that** email. Codex has no in-session `/login`.
3. `headroom` — tab to the Codex page.

Done when the row shows a plan; usage reads *unknown* until the first
refresh. A home whose login lives in Codex's keyring credential store shows
as not logged in — headroom reads only `auth.json`, and `headroom check`
reports the disagreement. Codex homes are retired by hand: `accounts remove
--vendor codex` refuses, naming the directory to delete once no `codex`
process runs.

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
`headroom launch --account <email>` starts. Folded-in sessions carry no
ownership evidence, so the picker resumes them on the current account until
one is re-homed there (`x` on its row).

Codex is the same topology with its own store: each extra home's `sessions/`
links to `~/.codex/sessions`, so Codex's own `resume` reaches any session
from any account. A home with a real `sessions/` directory is refused by
`headroom launch --vendor codex` the same way; fold it in with no `codex`
running.

## Retire a subscription

`headroom accounts remove <email>` (bare, it offers a picker of the
removable accounts) — refuses while that account has a live session or an
unmigrated `projects/`, asks `y/N` (`--yes` in scripts), deletes the
account's own Keychain item and dir, scrubs `.order`. Transcripts survive.
Done when `headroom` no longer lists the account; if the default pointed
there, `headroom accounts` repicks. The same command clears stranded
`<name>.lock` debris.

## Shell launchers (optional)

Short names over the engine — the shell owns the spelling, headroom owns
the routing. A wrapper passes names and flags and nothing else:
`CLAUDE_CONFIG_DIR`, `.current` and every check stay in `headroom launch`,
re-resolved from PATH at each keystroke, while a shell function is frozen
at shell init. Codex gets the same pair over `headroom launch --vendor
codex` and `HEADROOM_CODEX_LAUNCHER_FORMAT`. The starter set (`x`, `xa`, `xacc` — the board, passed
`--compact` for one row per account — and `xs` with the cd that outlives
the session) and `HEADROOM_LAUNCHER_FORMAT`, which makes the board
advertise those names, are in the repo's docs/REFERENCE.md § Shell integration
(github.com/qiushiyan/headroom/blob/main/docs/REFERENCE.md#shell-integration);
copy them into the
user's shell rc and adapt the names.
