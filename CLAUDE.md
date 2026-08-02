# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`headroom` answers one question — **which of this machine's Claude Code accounts has quota left** — and acts on the answer. Several Claude Code logins coexist, one per config dir; `headroom` renders every account's live limit bars in one view, `headroom select` is an interactive picker that records the chosen account for the shell to launch, and `headroom check` verifies the reverse-engineered vendor facts everything rests on. The mental model and those vendor contracts live in [DESIGN.md](DESIGN.md) — read it before touching the parsers, `check`, or anything Keychain-related.

## Commands

```bash
make build            # go build ./...
make install          # build → ~/.local/bin/headroom
make test             # go test ./...
make vet / make fmt
make check            # vet + test

go test ./internal/usage -run TestDriftTags   # single test
```

Interactive picker behavior (raw mode, signals) is out of `go test`'s reach — verify it with a PTY/expect harness; see DESIGN.md § Verification.

## Architecture

One pipeline, three commands over it:

`config` (paths; `HEADROOM_*` env overrides) → `accounts` (discover config dirs — the filesystem is the registry, no account list exists anywhere) → `creds` (Keychain credential blob per account) → `usage` (parallel fetches of the usage endpoint) → `render` (bars and status lines).

The dashboard prints the pipeline's result. `select` runs the same pipeline into a picker — `internal/tui` owns the raw-terminal session (restoration, signals, key decoding), `app/select.go` owns the layout — and commits the choice by writing the `.current` state file. `check` is the strict twin of the tolerant renderer.

Load-bearing choices:

- **`creds.Parse` and `usage.ParseLimits` are the only readers of vendor data.** Renderer and checker share them: rendering tolerates malformed fields (degrades to `0%` / `resets ?`), but every degraded field carries an ok/none/bad tag and `check` fails on any `bad` — drift the renderer papers over still gets caught. Never add a second parse path.
- **Read-only invariant**: the Keychain is never written; token refresh belongs to Claude Code. The single file headroom writes is `.current`, atomically (temp file + rename).
- **Accounts fail independently**: any per-account problem becomes that account's rendered status line, never a process failure.
- **stdlib + `golang.org/x/term` only** — no CLI/TUI frameworks, no cgo. Parsing is pure functions tested by table; exec and HTTP stay at the edges (`internal/app` wires them).
