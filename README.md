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
| `headroom` | Live limit bars for every account, fetched in parallel |
| `headroom --json` | The same data as a versioned JSON document, for scripts and status lines |
| `headroom select` | Interactive picker; records the chosen account for the shell's bare launcher |
| `headroom watch` | The dashboard, refreshed on a deliberately lazy interval |
| `headroom check` | Verifies the reverse-engineered assumptions still hold (run after a Claude Code update) |

## How it works

There is no account list: Claude Code's default `~/.claude` plus every
directory under `~/.claude-accounts` (one per extra login, named by its
email) *is* the account set. Claude Code keys its macOS Keychain credentials
per config dir, so every dir is an independent login and all tokens coexist.
headroom reads each account's credentials, calls the same usage endpoint
Claude Code's own `/usage` screen calls, and renders the result.

headroom is strictly **read-only** toward that system: it never writes the
Keychain and never refreshes a token — Claude Code owns both. The one file
it ever writes is `.current` (from `select`), which names the account a
shell launcher should target. The launcher commands it advertises
(`x-<name>`) are provided by shell integration, not by headroom; the
contract between the two is spelled out in [DESIGN.md](DESIGN.md), along
with the reverse-engineered vendor facts everything rests on.

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
| `HEADROOM_ACCOUNTS_ROOT` | `~/.claude-accounts` | Where the extra account dirs live |
| `HEADROOM_PRIMARY_NAME` | `qiushi` | Launcher name advertised for the primary `~/.claude` |

## Status

The usage endpoint headroom reads is undocumented, and the vendor's consumer
terms restrict it to first-party clients. This repository exists as a
personal, read-only instrument — source is public for reading, but there are
no releases and no packaging, and none are planned. Everything it assumes
about Claude Code is reverse-engineered and perishable; `headroom check`
exists precisely because any Claude Code update may break it.
