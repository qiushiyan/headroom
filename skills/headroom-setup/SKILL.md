---
name: headroom-setup
description: Set up headroom for several Claude Code or Codex subscriptions — install, add and log in each account, share config, keep sessions machine-global, give a program its own sessions on headless tokens, retire an account, shell launchers.
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
   of a config package instead. Either way a Claude Code account gets the
   primary's `settings.json`: `headroom check` fails unless every account
   sharing the session store reads one.
2. Hand the user: `headroom launch --account <email>`, then `/login` in that
   session choosing **that** email.
3. `headroom` — the board.

Done when the new row shows a plan and bars with no red `(dir says …!)` —
that warning means the dir was logged in as the wrong account; `headroom
login <email>` again. Repeat per subscription. Bare launches choose the
least-loaded account; an account not yet logged in is left out of that
choice until it is. An account bought
for one job is kept to it by `headroom accounts reserve <email> <dir>...`. The primary needs nothing; its board name is
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

## A program that runs its own sessions

For a program that starts `claude` under a config dir of its own, or under
another `HOME` — a pipeline, an eval runner. It spends the subscriptions
through headless tokens held by this home, never through logins of its own,
which would be a second set to renew.

1. Hand the user, per account the program may spend: `headroom headless
   mint <email>`. It needs a terminal; the approval opens in the account's
   Chrome profile (over ssh, at the printed URL with the code pasted back).
   The token is stored in a file only the user can read and is never
   printed.
2. The program, per session: `headroom headless place` — JSON naming the
   account and a placement id, exit 3 when every account it could take is
   near a limit; `headroom headless token <account>` into
   `CLAUDE_CODE_OAUTH_TOKEN`; then `headroom headless started <placement>
   <pid>` with the session's process (a launcher that execs `claude`
   reports its own pid). Under another `HOME` it sets `HEADROOM_HOME` to
   this home first. The repo's docs/specs/headless.md is the program's
   contract.

Done when `headroom headless`, run from the program's environment, shows
each account with its token and when it ends.

## Sessions are machine-global

`accounts add` links each account's `projects/` to `~/.claude/projects`, so
every account sees every conversation: `headroom launch -- --resume <id>`
from the session's project continues it on the account that last drove it,
and `--account <name>` moves it. Accounts seeded this way are done.

An account dir that predates headroom with a *real* `projects/` directory
holds sessions only it can see, and `headroom launch` refuses it. Fold them
in with no `claude` running: move each `<dir>/projects/<project>/` into
`~/.claude/projects/<project>/` (same names merge). A file both trees hold
is dropped from the account's tree only when the two are byte-identical
(`cmp`); different contents are two histories of one session — neither
timestamp nor size says one contains the other — so stop, keep both, and
hand them to the user to reconcile. Only once nothing is left under
`<dir>/projects`: `rmdir <dir>/projects && ln -s ~/.claude/projects
<dir>/projects`. Done when `headroom check` passes topology and
`headroom launch --account <email>` starts. Folded-in sessions carry no
ownership evidence, so a resume places each like a new session; the
account it runs on then owns it.

Every account's Claude Code cleanup sweep prunes that one store with its own
`settings.json`'s `cleanupPeriodDays`, so the shortest wins. Once a second
account shares the store, `headroom check` fails its `settings:` line unless
every sharing account (the primary included) reads one `settings.json`, and
its `retention:` line when they disagree or when `cleanupPeriodDays` is
unset — the default is Claude Code's to change. `accounts add` links the
primary's `settings.json` into the account whenever the share does not
provide one; set `cleanupPeriodDays` there to the period the user wants
kept. A dir that predates that link is linked by hand.

Codex is the same topology with its own store: each extra home's `sessions/`
links to `~/.codex/sessions`, so Codex's own `resume` reaches any session
from any account. A home with a real `sessions/` directory is refused by
`headroom launch --vendor codex` the same way; fold it in with no `codex`
running.

## Retire a subscription

`headroom accounts remove <email>` (bare, it names the removable accounts
and refuses) — refuses while that account has a live session or an
unmigrated `projects/`, asks `y/N` (`--yes` in scripts), deletes the
account's own Keychain item and dir, scrubs `.order` and its reservation.
Transcripts survive.
Done when `headroom` no longer lists the account. The same command clears
stranded
`<name>.lock` debris.

## Shell launchers (optional)

Short names over the engine — the shell owns the spelling, headroom owns
the routing. A wrapper passes names and flags and nothing else:
`CLAUDE_CONFIG_DIR` and every check stay in `headroom launch`,
re-resolved from PATH at each keystroke, while a shell function is frozen
at shell init. Codex gets the same pair over `headroom launch --vendor
codex` and `HEADROOM_CODEX_LAUNCHER_FORMAT`. The starter set (`x` for the least-loaded account, `xa <account>`
for one session there, `xacc` for the board passed `--compact`, one row per
account) and `HEADROOM_LAUNCHER_FORMAT`, which makes the board
advertise those names, are in the repo's docs/REFERENCE.md § Shell integration
(github.com/qiushiyan/headroom/blob/main/docs/REFERENCE.md#shell-integration);
copy them into the
user's shell rc and adapt the names.
