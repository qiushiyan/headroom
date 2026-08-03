# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`headroom` answers two questions and acts on the answers: **which of this machine's Claude Code accounts has quota left** (`headroom accounts`, the board: live limit bars, refreshing while it is open, enter records the chosen account for the shell to launch) and **which session do I resume, on which account** (`headroom resume`: every session on the machine in one picker — transcripts are machine-global — resumed in its own project dir on the account that last drove it). `headroom check` verifies the reverse-engineered vendor facts both rest on. The mental model and those vendor contracts live in [DESIGN.md](DESIGN.md) — read it before touching the parsers, `check`, the session store, or anything Keychain-related.

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

Raw-mode terminal behavior (picker interaction, self-refresh, signal-time
restoration) is out of `go test`'s reach — `make test-pty` drives it through
a real pty; see DESIGN.md § Verification and `test/pty/run.sh`.

## Architecture

One pipeline; every command is a surface over it:

`config` (paths; `HEADROOM_*` env overrides) → `accounts` (discover config dirs — the filesystem is the registry, no account list exists anywhere; also parses `.claude.json` for the logged-in email *and* Claude Code's own cached usage payload) → `auth` (`claude auth status --json`, the health oracle) + `creds` (Keychain credential blob) → `state` (claims the per-account request budget; also replays the newest response it stored) → `usage` (fetches, for accounts the claim permitted) → `render` (bars and status lines).

`accounts` runs the pipeline into a picker that re-runs it at the endpoint's own cadence — `internal/tui` owns the raw-terminal session (restoration, signals, physical-key decoding; surfaces own their keymaps), `app/accounts.go` owns the layout and the refresh loop — and commits the choice by writing `.current`. Off a terminal it prints one frame instead; `--json` serializes the same result (versioned schema, drift tags exposed). `check` is the strict twin of the tolerant renderer.

The session surface is a second, network-free pipeline: `internal/sessions` walks `~/.claude/projects` (tail-parses each transcript for title/preview/cwd/model, attributes it to an account via per-account `history.jsonl` + the live-session registry + explicit re-homes from the state file), and `app/resume.go` renders it on `/dev/tty` (alt screen) while stdout carries one `dir\tid\tconfig-dir` decision line for the shell wrapper, which cds and launches. Owner semantics, the cd-verification rule, liveness gating and the mutation exception are specified in DESIGN.md § The session surface.

Fetch results flow through a channel to a single writer: a goroutine fetches, classifies and records the outcome in the store, and only the consuming loop touches views (`resolve`), so a redraw may read every view between receives. Goroutines are handed copies of what they need and never see an `accountData`, which makes the property structural; `TestLaunchFetchesSingleWriter` guards it under `-race`.

Load-bearing choices:

- **An account has three independent axes, never one status.** `Health` (can Claude Code use it — only `/login` fixes a bad answer), `Observation` (rows, always carrying `ObservedAt` and `Source`), `Attempt` (how the newest request went — never a statement about the account). Collapsing them is the bug this model exists to prevent: a 429 or an aged-out access token must annotate what is known, never erase it or read as account failure. Rows never travel without their timestamp.
- **The two credential expiries mean different things.** `expiresAt` (~8h) aging out is routine — Claude Code refreshes it silently, and the correct response is "any `x-…` session refreshes it", never "/login". Only `refreshTokenExpiresAt` (~30d) passing means a human must act, and only on positive evidence: an absent or unparseable field is never read as expired.
- **One parser per vendor document type; no duplicate parse paths.** `creds.Parse`, `usage.ParseLimits` (live *and* Claude Code's cached copy — identical shape), `auth.Parse`, and the session readers `sessions.ParseTail`, `sessions.ParseHistory` and `sessions.ReadRegistry` are the only readers of vendor data. Attribution and title checks compare *decoded fields*, never substrings — transcript and prompt bodies quote UUIDs and record-type strings verbatim, and a substring match has already caused both a mis-attribution and a false `check` FAIL. Renderer and checker share them: rendering tolerates malformed fields without dropping the account, and degrades *visibly* — a bad percent renders as a `?` bar with a drift marker, never as a `0%` that reads like free headroom. Every degraded field carries a `tag.State`; `--json` exposes the tags, and `check` fails on any `bad`.
- **The usage endpoint's budget is per account** (~1/min), and headroom's own surfaces are its main consumer. `state.Claim` is the single authorization: it decides eligibility and records the claim inside one lock, so the permit it returns — never a flag computed earlier — is what lets a request leave. Deciding eligibility anywhere else reopens the window where two processes both fetch. Backoff means no traffic; a refused request may itself count against the budget. Never add a retry inside a run.
- **headroom stores what it bought, not just what it spent.** A run inside its quiet period replays its own newest response rather than falling back to Claude Code's cache, which is written only when Claude Code itself fetches and is routinely a day old. Bodies are stored raw so `usage.ParseLimits` stays their only reader. See DESIGN.md § The one file headroom writes for the corruption policy — the ledger is disposable and self-heals quiet, re-homes are human decisions and are never written over.
- **Read-only against Claude Code, with two documented exceptions**: the Keychain is never written, token refresh belongs to Claude Code, login and quota state are never touched. headroom writes two files of its own, atomically: `state.json` (its request ledger, the responses it fetched, and explicit session re-homes only — ordinary resumes write nothing; the launch itself becomes vendor evidence) and `.current`, which stays outside it because the shell reads *and* writes that one. The exceptions are the resume picker's `r` (appends one vendor-format `custom-title` record) and `dd` (deletes a transcript); both are explicit user commands and both are refused while the session is live or liveness is unverifiable.
- **A session's owner is evidence, newest-wins, never re-stamped.** Verified live registry > newest of {explicit re-home, newest history prompt}; degraded attribution (no evidence, deleted account, same-instant conflict) falls back to the current account *visibly*. Resume never writes `.current` — that file means "where new sessions go" and only `headroom accounts` moves it.
- **Accounts fail independently**: any per-account problem becomes that account's rendered status line, never a process failure.
- **`check` distinguishes "broken" from "couldn't test"**: PASS / FAIL / INCONCLUSIVE (exit 0/1/2). Rate limiting, transport failure and a stale access token are inconclusive, not drift. It also separates its own files from the vendor's: a corrupt `state.json` must never report as "Claude Code changed a format".
- **stdlib + `golang.org/x/term` only** — no CLI/TUI frameworks, no cgo. Parsing is pure functions tested by table; exec and HTTP stay at the edges (`internal/app` wires them).
