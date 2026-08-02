# Design — why headroom is shaped this way

headroom is a read-only observer of a system it doesn't own: several Claude
Code logins on one machine, each keyed to its own config dir. It answers
"which account has headroom left?" (the dashboard), lets the user act on the
answer (`select`), and proves its own assumptions still hold (`check`). This
file records the mental model and the vendor contracts — the things the code
can't say about itself.

## The system observed: the filesystem is the registry

There is no account list. Claude Code's default config dir (`~/.claude`)
plus every directory under the accounts root (default `~/.claude-accounts`,
one dir per extra login, named by its email) *is* the account set.
Everything derives from that tree:

- **Isolation — the load-bearing vendor fact.** Claude Code honors
  `CLAUDE_CONFIG_DIR` and keys its macOS Keychain item *per config dir*:
  service `Claude Code-credentials` for the default dir, with
  `-<sha256(dir)[0:8]>` appended for any other (verified against the binary,
  v2.1.220). Every dir is therefore an independent login, and all tokens
  coexist.
- **Labels.** Each dir's `.claude.json` records the email actually logged in
  (`.oauthAccount.emailAddress`). The dashboard warns when that contradicts
  the dir's name — it means `/login` picked the wrong account in that dir's
  session.
- **Two state files are the integration surface with the user's shell.**
  `.current` names the account the shell's bare launcher should target;
  `select` writes it atomically, and it is the *only* write headroom ever
  makes. `.order` (optional; one email per line, `#` comments) sets display
  order after the primary; unlisted accounts follow alphabetically.

Read-only is an invariant with that one stated exception: headroom never
writes the Keychain and never refreshes a token — Claude Code owns both. An
expired token's remedy is opening a Claude Code session on that account.

## The data source, and drift as a design input

The dashboard calls the endpoint Claude Code's own `/usage` screen calls —
`GET https://api.anthropic.com/api/oauth/usage` with a Bearer token from the
Keychain (falling back to a `.credentials.json` file where no keychain
exists). The endpoint is **undocumented and has already drifted once**
(legacy `five_hour`/`seven_day` fields giving way to the authoritative
`limits[]` array). Every fact in this file is reverse-engineered; treat all
of it as perishable. That expectation shapes the core seam:

- **Exactly two parsers.** `creds.Parse` (credential blob) and
  `usage.ParseLimits` (usage response) are the only readers of vendor data.
  The renderer and the checker share them, so the checker cannot drift from
  what rendering actually needs.
- **Tolerant rendering, tagged degradation.** A malformed field degrades
  instead of dropping the account, and degrades visibly — a bad percent is a
  `?` bar with a drift marker, never a `0%` that reads like free headroom;
  accounts fail independently; and every parsed field carries a state tag
  that distinguishes *legitimately absent* (an untouched limit window) from
  *present but no longer parseable* (shape drift). `--json` carries the same
  tags outward, so machine consumers can't mistake drift for data either.
- **`check` is the strict twin.** Per logged-in account it asserts the
  Keychain item under the predicted service name, the blob contract, and a
  live HTTP 200 with at least one limit row and zero bad tags; it also greps
  the installed Claude Code binary for the endpoint / config-dir /
  credential seams. A FAIL names which assumption broke. Run it after any
  Claude Code update, or whenever the dashboard misbehaves.

Two deliberate tolerances beyond strict inherited behavior: numeric-epoch
`resets_at` values are accepted, and timestamps may carry offsets or
fractional seconds. Handled drift beats flagged drift.

The same posture bounds `watch`: countdowns re-render every second from
cached data, but fetch rounds run one at a time on an interval with a hard
30-second floor, backing off on 429 — headroom never polls faster than the
first-party client's human-driven rate.

## The launcher contract

headroom launches nothing. It *advertises*, per account, the command a
shell-launcher family is expected to provide: `x-<email>` as the guaranteed
identity, with a short `x-<local-part>` alias only when the local part is
unique among accounts, isn't the primary account's name, and isn't a
reserved utility name (`usage`, `account`, `account-add`, `select`) — the
rule and list live in `accounts.Launcher`. Shell integration that generates
the actual functions must apply the same rule, and must read `.current` the
way headroom writes it: the account's dir name (or the primary's configured
name), newline-terminated, renamed into place atomically. The picker's job
ends at that state write — launching interactive sessions belongs to the
shell.

## Dependencies and shape

stdlib plus `golang.org/x/term` (raw-mode terminal control for the picker),
nothing else: no CLI framework, no TUI framework — the picker is a small
redraw loop. No cgo either: the Keychain is read by exec'ing `security(1)`,
which keeps builds trivial. Parsing is pure functions (`[]byte` in, tagged
structs out) tested by table; exec and HTTP stay in thin edges.

`internal/tui` owns the terminal session whole: raw-mode lifetime,
restoration on SIGTERM/SIGHUP/SIGINT (restore first, then re-raise so the
exit status stays honest about the signal), and incremental key decoding —
escape sequences may split across reads, and a bare ESC resolves as a
keypress only after a pause.

## Verification

Contract behavior is table-tested (`go test -race ./...`), drift tags
included; the fetch pipeline's single-writer property has a dedicated
regression test under the race detector. What `go test` can't reach —
signal-time terminal restoration, real picker and watch interaction — lives
in the committed expect(1) harness (`make test-pty`, `test/pty/`): SIGTERM
must leave the terminal sane; arrows + enter must select and write state;
ESC must cancel writing nothing; watch must draw and quit cleanly. Its two
hard-won patterns are documented in `test/pty/run.sh` — kill with
`pkill -nx headroom`, never `-f`, and inspect post-mortem terminal state via
an sh wrapper running `stty` in the same pty.

## Status

Personal-first: the defaults fit the author's machine, and `HEADROOM_*`
environment variables re-point each of them. The endpoint headroom reads is
undocumented and restricted by the vendor's consumer terms to first-party
clients; this repository exists as a personal, read-only instrument, not as
a distribution.
