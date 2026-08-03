# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`headroom` answers two questions and acts on the answers: **which of this machine's Claude Code accounts has quota left** (the dashboard; `headroom select` is an interactive picker that records the chosen account for the shell to launch) and **which session do I resume, on which account** (`headroom resume`: every session on the machine in one picker — transcripts are machine-global — resumed in its own project dir on the account that last drove it). `headroom check` verifies the reverse-engineered vendor facts both rest on. The mental model and those vendor contracts live in [DESIGN.md](DESIGN.md) — read it before touching the parsers, `check`, the session store, or anything Keychain-related.

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

One pipeline; every command is a surface over it:

`config` (paths; `HEADROOM_*` env overrides) → `accounts` (discover config dirs — the filesystem is the registry, no account list exists anywhere; also parses `.claude.json` for the logged-in email *and* Claude Code's own cached usage payload) → `auth` (`claude auth status --json`, the health oracle) + `creds` (Keychain credential blob) → `throttle` (per-account request eligibility) → `usage` (fetches, for accounts that are eligible) → `render` (bars and status lines).

The dashboard prints the pipeline's result; `--json` serializes the same result (versioned schema, drift tags exposed). `select` runs the pipeline into a picker — `internal/tui` owns the raw-terminal session (restoration, signals, physical-key decoding; surfaces own their keymaps), `app/select.go` owns the layout — and commits the choice by writing the `.current` state file. `watch` re-runs it on a floored, backoff-guarded interval with per-second local redraws. `check` is the strict twin of the tolerant renderer.

The session surface is a second, network-free pipeline: `internal/sessions` walks `~/.claude/projects` (tail-parses each transcript for title/preview/cwd, attributes it to an account via per-account `history.jsonl` + the live-session registry + the `.owners` re-home file), and `app/resume.go` renders it on `/dev/tty` (alt screen) while stdout carries one `dir\tid\tconfig-dir` decision line for the shell wrapper, which cds and launches. Owner semantics, the cd-verification rule, liveness gating and the mutation exception are specified in DESIGN.md § The session surface.

Fetch results flow through a channel to a single writer: goroutines produce `usage.Result`s, only the consuming loop touches views (`resolve`), so a redraw may read every view between receives. `TestLaunchFetchesSingleWriter` guards this under `-race`.

Load-bearing choices:

- **An account has three independent axes, never one status.** `Health` (can Claude Code use it — only `/login` fixes a bad answer), `Observation` (rows, always carrying `ObservedAt` and `Source`), `Attempt` (how the newest request went — never a statement about the account). Collapsing them is the bug this model exists to prevent: a 429 or an aged-out access token must annotate what is known, never erase it or read as account failure. Rows never travel without their timestamp.
- **The two credential expiries mean different things.** `expiresAt` (~8h) aging out is routine — Claude Code refreshes it silently, and the correct response is "any `x-…` session refreshes it", never "/login". Only `refreshTokenExpiresAt` (~30d) passing means a human must act, and only on positive evidence: an absent or unparseable field is never read as expired.
- **One parser per vendor document type; no duplicate parse paths.** `creds.Parse`, `usage.ParseLimits` (live *and* Claude Code's cached copy — identical shape), `auth.Parse`, and the session readers `sessions.ParseTail`, `sessions.ParseHistory` and `sessions.ReadRegistry` are the only readers of vendor data. Attribution and title checks compare *decoded fields*, never substrings — transcript and prompt bodies quote UUIDs and record-type strings verbatim, and a substring match has already caused both a mis-attribution and a false `check` FAIL. Renderer and checker share them: rendering tolerates malformed fields without dropping the account, and degrades *visibly* — a bad percent renders as a `?` bar with a drift marker, never as a `0%` that reads like free headroom. Every degraded field carries a `tag.State`; `--json` exposes the tags, and `check` fails on any `bad`.
- **The usage endpoint's budget is per account** (~1/min), and headroom's own surfaces are its main consumer. `internal/throttle` records eligibility per account, across processes, *before* each request. Backoff means no traffic — a refused request may itself count against the budget. Never add a retry inside a run.
- **Read-only against Claude Code, with two documented exceptions**: the Keychain is never written, token refresh belongs to Claude Code, login and quota state are never touched. headroom writes three files of its own, atomically: `.current`, `.throttle` and `.owners` (explicit session re-homes only — ordinary resumes write nothing; the launch itself becomes vendor evidence). The exceptions are the resume picker's `r` (appends one vendor-format `custom-title` record) and `dd` (deletes a transcript); both are explicit user commands and both are refused while the session is live or liveness is unverifiable.
- **A session's owner is evidence, newest-wins, never re-stamped.** Verified live registry > newest of {explicit re-home, newest history prompt}; degraded attribution (no evidence, deleted account, same-instant conflict) falls back to the current account *visibly*. Resume never writes `.current` — that file means "where new sessions go" and only `headroom select` moves it.
- **Accounts fail independently**: any per-account problem becomes that account's rendered status line, never a process failure.
- **`check` distinguishes "broken" from "couldn't test"**: PASS / FAIL / INCONCLUSIVE (exit 0/1/2). Rate limiting, transport failure and a stale access token are inconclusive, not drift.
- **stdlib + `golang.org/x/term` only** — no CLI/TUI frameworks, no cgo. Parsing is pure functions tested by table; exec and HTTP stay at the edges (`internal/app` wires them).
