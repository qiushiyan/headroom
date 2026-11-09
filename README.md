# headroom

Several Claude Code subscriptions on one machine, each logged in under its
own config dir. headroom is the instrument for that setup: it shows which
account has quota left, starts each session on the least-loaded account (or
the one you name), and lets a conversation move between accounts — so a rate
limit on one subscription stops nothing. It does the same for several Codex
CLI subscriptions: the board gets a Codex page, and the same launcher starts
`codex`.

```
alice@example.com (max 20x · headroom launch --account alice@example.com)
  5h session       [███░░░░░░░░░░░░░░░░░]  17%  resets 03:19 (in 2.1h)
  All models (7d)  [████████████░░░░░░░░]  61%  resets Thu 17:59 (in 4.7d)

bob@example.com (max 5x · headroom launch --account bob@example.com)  ← next
  5h session       [█████████░░░░░░░░░░░]  45%  resets 01:50 (in 0.6h)
  All models (7d)  [█░░░░░░░░░░░░░░░░░░░]   6%  resets Mon 17:00 (in 1.6d)
```

## Use cases

### 1. See how much each account has left

`headroom` is the board: every account's 5-hour and weekly limits as live
bars, refreshing while it stays open, with the command that launches each
account and `← next` on the one a bare launch would take; no key on it
changes where a launch goes. It reads the same endpoint Claude Code's own
`/usage` screen reads, subject to a shared per-account request budget. A deferred or failed refresh keeps the newest
usable observation, with its age and request status alongside the figures.
Severity colours remain visible when data is stale.

`headroom limits` always emits JSON from disk, for scripts and status lines.
`headroom --json` probes and refreshes within the same request budget as
the board.

### 2. Spread sessions across accounts

Each subscription has its own 5-hour window. A bare `headroom launch` starts
`claude` on the least-loaded account; `headroom launch --account <email>`
starts one session on the account named. Anything after `--` goes to
`claude`. Every launch says which account it took before `claude` starts:

```
headroom launch: alice@example.com · auto · 5h 1% · week 3%, resets in 4.2d · load 0 (next: bob@example.com)
```

Load is five-hour usage plus the sessions busy on that account and the
launches just made there; an account near any limit is set aside, and among
equally loaded accounts the weekly room that would lapse first is spent
first. Launches started together land on different accounts.
`headroom launch --dry-run` shows the whole table without starting anything,
and each launch leaves one line in `~/.claude-accounts/launches.jsonl` for
reading later with `jq`.

An account bought for one job can be kept to it: `headroom accounts reserve
<email> ~/dev/work ~/dev/work-cms`. Automatic launches elsewhere never choose
it, and in those repositories and their worktrees it is ranked like any other.

Every session, whichever account started it, lives in one machine-wide
store, so any account can continue it. `headroom launch -- --resume <id>`
from the session's project continues it on the account that last drove it,
while that account has room.

### 3. Out of quota mid-session? Continue on another account

A session on account A hits its limit with work unfinished. The transcript
belongs to the machine, not the account, so any account can pick it up: quit
the session and, from its project,
`headroom launch --account <email> -- --resume <id>` continues it on the
account named — or `-- --continue` for the newest session in that directory.
A bare `headroom launch -- --resume <id>` moves it off A by itself once A is
near a limit. Ownership follows automatically: the account that drives a
session last is its owner.

### 4. The same, for Codex

With Codex on the machine the board has a second page — **tab** switches:

- `headroom launch --vendor codex` — `codex` on the least-loaded Codex
  account; `--account <email>` for one session on the account named.
- Out of quota mid-session: Codex's sessions are shared across its accounts,
  so continuing elsewhere is naming the account —
  `headroom launch --vendor codex --account <other> -- resume` (add `--all`
  for every project's sessions).

A Codex account reads *unknown* until headroom's first fetch, and *blocked*
when the vendor says so, whatever its percentages.

### 5. Renew every login before it ends

A Claude Code login lasts about a month from the day it was made, and every
account's runs out on its own day. `headroom login` renews the ones that end
within a week — `--all` renews every account, so they end together. Each
account's approval page opens in the Chrome profile named after it (or signed
in to its Google account); you click Authorize, and headroom checks the dir
is now logged in as that account:

```
→ alice@example.com     login ends Oct 23 (in 6d) — Chrome profile alice (Profile 5)

[1/1] alice@example.com — click Authorize in the page that opens
✓ alice@example.com: logged in, login ends Nov 21 (in 29d)

the logins renewed here end from Nov 21 (in 29d)
```

Away from this machine's screen — an agent with shell access, or you on
another machine — `headroom login start <account>` prints the sign-in URL as
JSON and opens nothing; approve it in a browser signed in as the account and
pipe the code the page shows into `headroom login finish <account>`, which
says whether the login took. See
[docs/REFERENCE.md](docs/REFERENCE.md#renewing-a-login-from-elsewhere).

### 6. A program that runs its own sessions

A pipeline or eval runner that runs `claude` under a config dir of its own
spends the subscriptions through long-lived tokens instead of logins:
`headroom headless mint <account>` once a year per account, then
`headroom headless place`, `token` and `started` per session, so its
sessions count as load the launcher steers around. A program under another
`HOME` reaches this home's headroom with `HEADROOM_HOME`. The contract is
[docs/specs/headless.md](docs/specs/headless.md).

## Install

```sh
go install github.com/qiushiyan/headroom/cmd/headroom@latest
headroom check      # PASS or INCONCLUSIVE on a fresh machine
```

macOS is the primary target. Headroom reads Claude Code's Keychain credential
first, then its `.credentials.json` file when the Keychain cannot supply one
(including SSH sessions with a locked login Keychain and Linux). Codex's
login is read from its `auth.json`; a Codex login kept in the keyring store
shows as not logged in. Go, `golang.org/x/term`, and clikit with Kong for the
command line.

## Set up your accounts

Your existing `~/.claude` login is the **primary**. Each further subscription
becomes one directory under `~/.claude-accounts/`, named by its email:

```sh
headroom accounts add alice@example.com --share-config   # seed the dir; share settings/skills/hooks from ~/.claude
headroom launch --account alice@example.com              # claude opens on that account — /login there, once
headroom                                                 # the new row is on the board
```

Repeat per subscription; that is the setup. Login state and history always
stay per account. `--share-config` is optional; either way the account reads
the primary's `settings.json`, because every account's cleanup sweep prunes
the one shared session store, and `headroom check` expects
`cleanupPeriodDays` set there. To retire one:
`headroom accounts remove <email>`.

Codex is the same shape — `~/.codex` is the primary, each further
subscription a home under `~/.codex-accounts/` — and logs in with one command
rather than from inside a session:

```sh
headroom accounts add --vendor codex alice@example.com --share-config   # seed the home; share config.toml, AGENTS.md, skills, …
headroom launch --vendor codex --account alice@example.com -- login     # log in as that email, once
```

headroom never removes a Codex home: it cannot tell whether a Codex session
is running on one, so `accounts remove --vendor codex` names the directory to
delete by hand.

Every command's reference is `headroom <command> --help`. **Shell shortcuts**
(`x`, `xa`, `xacc`, …) and what holds across commands live in
[docs/REFERENCE.md](docs/REFERENCE.md).

## For your agent

The repo ships two agent skills — `headroom` (reading the board, launching,
the out-of-quota flow, diagnosing) and `headroom-setup` (the setup above,
plus folding in older session dirs and shell launchers):

```sh
npx skills add qiushiyan/headroom
```

## Status

Everything headroom assumes about Claude Code and Codex — Keychain naming,
`auth.json`, the usage endpoints and their response shapes, the session store
layouts — is reverse-engineered and perishable. `headroom check` exists
precisely because any vendor update may break it; run it when something looks
wrong. The usage endpoints are undocumented and the vendors' consumer terms
restrict them to first-party clients: headroom reads them the way the vendors'
own usage screens do and never writes login or quota state. Install it as a read-only
instrument you understand, not as a supported product — there are no
releases; `go install` from source is the distribution.

How it works, shell integration, configuration, developing:
[docs/REFERENCE.md](docs/REFERENCE.md) · design and vendor contracts:
[DESIGN.md](DESIGN.md).
