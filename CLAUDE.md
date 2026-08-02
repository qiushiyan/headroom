# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`headroom` answers one question — **which of this machine's Claude Code accounts has quota left** — and acts on the answer. Several Claude Code logins coexist, one per config dir; `headroom` renders every account's live limit bars in one view, `headroom select` is an interactive picker that records the chosen account for the shell to launch, and `headroom check` verifies the reverse-engineered vendor facts everything rests on. The mental model and those vendor contracts live in [DESIGN.md](DESIGN.md) — read it before touching the parsers, `check`, or anything Keychain-related.

## Commands

```bash
make build            # go build ./...
make install          # build → ~/.local/bin/headroom
make test             # go test -race ./...
make vet / make fmt
make check            # vet + test
make test-pty         # expect(1) harness for the interactive surface

go test ./internal/usage -run TestDriftTags   # single test
```

Raw-mode terminal behavior (picker interaction, watch, signal-time
restoration) is out of `go test`'s reach — `make test-pty` drives it through
a real pty; see DESIGN.md § Verification and `test/pty/run.sh`.

## Architecture

One pipeline, five surfaces over it:

`config` (paths; `HEADROOM_*` env overrides) → `accounts` (discover config dirs — the filesystem is the registry, no account list exists anywhere) → `creds` (Keychain credential blob per account) → `usage` (parallel fetches of the usage endpoint) → `render` (bars and status lines).

The dashboard prints the pipeline's result; `--json` serializes the same result (versioned schema, drift tags exposed). `select` runs the pipeline into a picker — `internal/tui` owns the raw-terminal session (restoration, signals, key decoding), `app/select.go` owns the layout — and commits the choice by writing the `.current` state file. `watch` re-runs it on a floored, backoff-guarded interval with per-second local redraws. `check` is the strict twin of the tolerant renderer.

Fetch results flow through a channel to a single writer: goroutines produce `usage.Result`s, only the consuming loop touches views (`resolve`), so a redraw may read every view between receives. `TestLaunchFetchesSingleWriter` guards this under `-race`.

Load-bearing choices:

- **`creds.Parse` and `usage.ParseLimits` are the only readers of vendor data.** Renderer and checker share them: rendering tolerates malformed fields without dropping the account, and degrades *visibly* — a bad percent renders as a `?` bar with a drift marker, never as a `0%` that reads like free headroom. Every degraded field carries an ok/none/bad tag; `--json` exposes the tags, and `check` fails on any `bad`. Never add a second parse path.
- **Read-only invariant**: the Keychain is never written; token refresh belongs to Claude Code. The single file headroom writes is `.current`, atomically (temp file + rename).
- **Accounts fail independently**: any per-account problem becomes that account's rendered status line, never a process failure.
- **stdlib + `golang.org/x/term` only** — no CLI/TUI frameworks, no cgo. Parsing is pure functions tested by table; exec and HTTP stay at the edges (`internal/app` wires them).
