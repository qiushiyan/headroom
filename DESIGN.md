# Design — why headroom is shaped this way

headroom is a read-only observer of a system it doesn't own: several Claude
Code logins on one machine, each keyed to its own config dir. It answers
"which account has headroom left?" and lets the user act on the answer (the
account board, `accounts`: live and self-refreshing on a terminal, one frame
off it, a versioned document under `--json`), answers "which session do I get
back into, and on which account?" (`resume` — see The session surface below),
and proves its own assumptions still hold (`check`). This file records the
mental model and the vendor contracts — the things the code can't say about
itself.

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
- **Labels, and a free usage cache.** Each dir's `.claude.json` records the
  email actually logged in (`.oauthAccount.emailAddress`); the board warns
  when that contradicts the dir's name — `/login` picked the wrong account in
  that dir's session. The same file carries `cachedUsageUtilization`: the last
  usage response *Claude Code itself* fetched, stamped with `fetchedAtMs`. Its
  `utilization` object is shape-identical to the live endpoint's body, so it
  feeds the same parser and costs no request. Freshness is uncontrolled — it
  is written only when Claude Code fetches, so ages range from minutes to a
  day, and an account never used has none at all. It is a fallback with
  honest provenance, never a substitute for asking.
- **Health comes from the vendor, not from arithmetic.**
  `claude auth status --json` answers "is this account logged in", per config
  dir, from local state: ~170ms, no network, no usage budget. headroom infers
  account health from credential timestamps only when that command can't
  answer.
- **Two files are the shell's, not headroom's.** `.current` names the account
  the shell's bare launcher should target — the board writes it atomically,
  and the shell's own per-account launchers rewrite it at every launch.
  `.order` (optional; one email per line, `#` comments) sets display order
  after the primary; unlisted accounts follow alphabetically. Both stay
  outside headroom's own state on purpose; see The one file headroom writes.

Read-only means read-only *against Claude Code*: headroom never writes the
Keychain, never refreshes a token, and never touches vendor login or quota
state — Claude Code owns all of it. What it writes is its own: `.current`,
and one `state.json` holding non-secret request timestamps, the usage
responses it fetched itself, and explicit session re-homes. The session
surface carries the two documented exceptions to the rule, both explicit user
commands against the shared transcript store: `r` appends one vendor-format
`custom-title` record (exactly what the native Ctrl+R rename writes), and `dd`
deletes a transcript. Both are refused while any process holds the session
open.

`claude auth status` is used for its answer only. It may or may not refresh a
token as a side effect; headroom neither relies on that nor invokes it hoping
for it. Building on an unpromised side effect would be the same class of
mistake as reading `expiresAt` as account health.

## The data source, and drift as a design input

The board calls the endpoint Claude Code's own `/usage` screen calls —
`GET https://api.anthropic.com/api/oauth/usage` with a Bearer token from the
Keychain (falling back to a `.credentials.json` file where no keychain
exists). The endpoint is **undocumented and has already drifted once**
(legacy `five_hour`/`seven_day` fields giving way to the authoritative
`limits[]` array). Every fact in this file is reverse-engineered; treat all
of it as perishable. That expectation shapes the core seam:

- **One parser per vendor document type, and no duplicate parse paths.**
  `creds.Parse` (credential blob), `usage.ParseLimits` (usage response, live
  *and* cached — they are the same shape) and `auth.Parse` (auth status) are
  the only readers of vendor data. Renderer and checker share them, so the
  checker cannot drift from what rendering actually needs.
- **Tolerant rendering, tagged degradation.** A malformed field degrades
  instead of dropping the account, and degrades visibly — a bad percent is a
  `?` bar with a drift marker, never a `0%` that reads like free headroom;
  accounts fail independently; and every parsed field carries a `tag.State`
  distinguishing *legitimately absent* (an untouched limit window, a
  credential field the vendor stopped sending) from *present but no longer
  parseable* (shape drift). `--json` carries the same tags outward, so machine
  consumers can't mistake drift for data either.
- **`check` has three outcomes, not two.** PASS means every assertion was
  tested and held. FAIL means one was tested and contradicted. INCONCLUSIVE
  (exit 2) means it could not be tested: rate limited, network down, or an
  access token mid-refresh. A checker that reports "Claude Code likely changed
  a format" because it couldn't reach the endpoint actively misleads the
  person running it *because* the board misbehaved — which is the exact moment
  it is reached for. For the same reason it separates failures in headroom's
  own files from vendor drift, and says so in the closing line when every
  failure was its own. It also respects the request budget rather than
  spending one to "diagnose" — and it stores what it bought, since it spends
  the same budget the board does.

Two deliberate tolerances beyond strict inherited behavior: numeric-epoch
`resets_at` values are accepted, and timestamps may carry offsets or
fractional seconds. Handled drift beats flagged drift.

## Three axes, because three things vary independently

The defect this design replaced was a single `Status` per account. Health,
what is known about quota, and how the last request went were one field, so
each overwrote the others — and a refused request erased known bars and read
as bad news about the account. All three can be true at once, and the model
now says so:

- **Health** — can Claude Code use this account? Only `/login` fixes a bad
  answer. Sourced from `claude auth status`, with credential evidence as
  fallback.
- **Observation** — rows, *always* carrying `ObservedAt` and `Source`. Rows
  never travel without their timestamp; that is what let carried-over data
  pass itself off as current. `Source` separates headroom's own fetch in this
  process from its own answer replayed off disk from Claude Code's cache: the
  first two are equally headroom's reading of the endpoint and the caption
  treats them alike, while a machine consumer can still tell "this run asked"
  from "a previous run asked". A row whose own reset instant has passed is not
  an answer at all — the window it describes has ended, so it renders as
  unknown rather than as a low percent that reads like free headroom, and it
  disqualifies the account from being called a live choice.
- **Attempt** — what happened to the newest request. Never a statement about
  the account.

Two consequences worth stating. An access token aging out is an *attempt*
fact: the token lives ~8 hours, Claude Code refreshes it silently, and the
account is fine — only `refreshTokenExpiresAt` passing means a human must act.
And a failed refresh annotates an observation rather than replacing it, so the
board degrades to "58%, observed 22h ago, refresh rate-limited" instead of
to a bare error.

## The one file headroom writes

The usage endpoint rate-limits **per account** — account A can be refused in
the same second B and C succeed — and refills in roughly a minute. There is no
`Retry-After` worth reading, and a refused request may itself count against
the budget, so probing to discover recovery can prevent it. Backoff therefore
means *no traffic*, never a faster loop.

headroom's own surfaces are the main consumer: the board, `--json` and `check`
each ask about every account, and they fan out in parallel, so without
coordination the whole fleet would go dark together rather than one account at
a time. Every process is short-lived and there is no daemon, so coordination
has to live on disk. `internal/state` owns that file — one JSON document under
the accounts root, written atomically under an flock, holding the request
ledger, the responses, and the session re-homes.

- **A claim is a test-and-set, and the claim is the authorization.**
  Eligibility is decided and the claim recorded inside one locked section, so
  the permit handed back — not a flag computed earlier — is what lets a
  request leave. Deciding eligibility in one place and recording it in
  another leaves the window where two processes both read "eligible" — and a
  design that has that window can only call itself best-effort. Every surface
  goes through the same call, `check` included: a second route to the endpoint
  would be a second way to double-spend. Failing to lock, decode or write
  denies, rather than falling through to "try anyway". A completion carries
  the generation its claim was issued under, so a slow request that outlived
  its claim is dropped instead of overwriting a newer answer.
- **The ledger keys on the account's own UUID**, from `.claude.json`, falling
  back to the dir name only when that file parsed and reported none. The
  budget is per account, so two config dirs logged into the same account share
  one bucket — and keying by identity *deletes* the guard against replaying a
  re-logged dir's old quota rather than adding one: a re-logged dir simply has
  a different key. The distinction between "parsed, no UUID" and "did not
  parse" is load-bearing rather than pedantic: Claude Code rewrites that file
  constantly, so a torn read would otherwise move the key to a second, empty
  bucket and spend the same account's budget twice within seconds. An account
  whose identity cannot be read is not asked about at all until it can.
- **It records what was bought, not only what was spent.** A ledger of
  requests alone leaves every run inside its quiet period with nothing of its
  own to show, falling back to Claude Code's cache — which is written only
  when Claude Code itself fetches, and has been observed 37 hours stale. So a
  deferred refresh replays headroom's own answer, usually seconds old, under
  its own provenance. The body is stored raw: `usage.ParseLimits` stays its
  only reader, and no second row shape exists to drift from it.
- **Corruption degrades per section, and the two sections are not alike.** The
  ledger is disposable: unreadable bytes are set aside under a name nothing
  reads, and every account starts one full cooldown quiet, so the file
  self-heals rather than bricking — and never by guessing "eligible", which is
  the guess that generates traffic. Re-homes are human decisions: an
  unreadable sessions section refuses mutation outright rather than being
  written over, and records are validated whole, because a section that
  decodes *around* a hollow record reads as healthy while routing silently
  ignores it. Sections this binary cannot decode at all are carried through
  writes verbatim, and a document from a newer headroom is read but never
  rewritten.
- **Records are bounded by age, never by which accounts a caller could see.**
  Sweeping by absence needs a complete account registry, and no caller can
  promise one: an account's key comes from a vendor file Claude Code rewrites
  constantly, so a torn read moves the key and the sweep deletes the live
  cooldown and stored answer of an account sitting right there.
- **`.current` and `.order` stay out of it.** `.current` has two writers, one
  of them a shell reading it with `$(<file)` at every launch; folding it in
  would put headroom on the critical path of starting Claude Code. `.order` is
  configuration a human edits, comments included.

Two policies fall out of the budget. Figures are labelled stale only once they
are older than the request spacing — inside that window no newer answer is
obtainable, so nagging about age would be nagging about something nobody can
act on. And the board's refresh cadence *is* eligibility: there is no interval
to configure, because asking sooner is refused and asking later says less
than it could. Presence bounds it: a board left open on a spare tab stops
asking after a few minutes without a keypress, since a surface that polls for
hours with nobody looking is the background daemon this design declines to
have, wearing a TUI. `r` refreshes what is eligible or arms the next round; it
never buys an exemption, because an override would spend against a bucket
where a refusal can itself cost more than it buys.

## The session surface

Session transcripts are machine-global — every account's `projects/`
symlinks to `~/.claude/projects` (the dotfiles repo owns that topology) — so
one picker over that tree sees every conversation regardless of account.
`headroom resume` is that picker. Its vendor contracts, all reverse-
engineered from the 2.1.220 store and all perishable:

- **Transcripts describe themselves, from the tail.** Titles live *inside*
  each `<uuid>.jsonl` as repeated `ai-title` records (latest wins) and
  `custom-title` records (user rename; outranks), previews as `last-prompt`
  records, and the newest assistant record's `message.model` names the model
  that last drove the session (`"<synthetic>"` marks vendor error
  placeholders, never a model, and is skipped); there is no session index
  file. The first pass reads
  `sessions.TailBudget` (64KB) from EOF — measured to resolve everything
  for almost every transcript — and when the cwd or the model is still
  unresolved (a session can end in one message bigger than the budget,
  and a window starting inside it skips it as a partial line) the window
  widens until both resolve or the file is exhausted: a resumable row
  rendered "dir gone" is the picker lying, and a silently blank model is
  the same lie smaller. `check` asserts the tail resolves the same title,
  cwd and model as a full-file parse for every oversized transcript —
  the assertion that fires the day a load-bearing record drifts out of
  reach. Listability is structural: top-level `<uuid>.jsonl` files are
  sessions; `<uuid>/` closure dirs hold subagent transcripts and are not.
- **The cd target is verified, never de-munged.** `claude --resume <id>`
  resolves only from the session's owning project dir, and store dir names
  munge `/` and `.` to `-` — lossily. The resume target is therefore the
  newest recorded `cwd`/`relocatedCwd` whose munge *equals* the store dir
  name. No verifiable cwd → the row says so and refuses to resume (worktree
  churn deletes project dirs constantly; a third of the store's rows point
  at dirs that no longer exist, and guessing would resume into the wrong
  project). `dd` is the honest follow-up for those.
- **Owner = the newest account claim headroom can observe or was explicitly
  given** — transcripts carry no account identity, so "the account that
  drove this session" is evidence, not ground truth, and the contract says
  so. Three sources, one precedence: a *verified* live-registry claim (that
  account runs it now) beats everything; otherwise the newest of the explicit
  re-home and each account's newest `history.jsonl` prompt for that session
  id — all timestamps from this machine's clock, on one axis, never
  re-stamped. Attribution parses the decoded `sessionId`
  field, never substrings: prompt bodies quote other sessions' UUIDs.
  `enter` resumes on the owner and writes nothing — the launch itself
  becomes vendor evidence. The override key re-homes: it records the one
  fact the vendor never will ("the user pointed this session here before
  prompting") and is superseded by any newer evidence. Degradation is
  visible: no evidence, a re-home to a deleted account, or a same-instant
  conflict each fall back to the current account under their own tag.
- **Liveness is pid + start instant, and it gates the mutations.** Each
  account dir's `sessions/<pid>.json` registers a running session. A claim
  counts only when the pid is alive *and* its kernel start time matches the
  registry's `startedAt` within tolerance — pids recycle. (Epoch ms, not
  the `procStart` string: that one is UTC-rendered while `ps` speaks local
  time, so string equality fails everywhere but UTC.) Live and
  unverifiable sessions refuse `dd` and `r`; deleting an open transcript
  loses the conversation to an unlinked inode.
- **headroom decides and records; the shell executes.** The TUI runs on
  `/dev/tty` (alternate screen, restored in the same signal path as raw
  mode); stdout carries exactly one decision line —
  `<project-dir>\t<session-id>\t<config-dir>`, empty config dir = primary —
  after the terminal is restored. The wrapper cds (the cd sticks) and
  launches with `CLAUDE_CONFIG_DIR`, deliberately *without* touching
  `.current`: `x-accounts` owns where new sessions go, resume never moves
  it, and when the two accounts differ the header says both. Paths that
  would break the line protocol make a row unlaunchable rather than being
  escaped. This surface does no network I/O and never spends the usage
  budget.

## The launcher contract

headroom launches nothing. It *advertises*, per account, the command a
shell-launcher family is expected to provide: `x-<email>` as the guaranteed
identity, with a short `x-<local-part>` alias only when the local part is
unique among accounts, isn't the primary account's name, and isn't a
reserved utility name (`usage`, `account`, `account-add`, `select`,
`accounts`, `acc`, `check`) — the rule and list live in `accounts.Launcher`.
Shell integration that generates the actual functions must apply the same
rule, and must read `.current` the way headroom writes it: the account's dir
name (or the primary's configured name), newline-terminated, renamed into
place atomically. The board's job ends at that state write — launching
interactive sessions belongs to the shell.

## Dependencies and shape

stdlib plus `golang.org/x/term` (raw-mode terminal control for the two
pickers), nothing else: no CLI framework, no TUI framework — the interactive
surfaces share one small in-place redraw loop. No cgo either:
the Keychain is read by exec'ing `security(1)`, which keeps builds trivial.
Parsing is pure functions (`[]byte` in, tagged structs out) tested by table;
exec and HTTP stay in thin edges.

`internal/tui` owns the terminal session whole: raw-mode lifetime,
restoration on SIGTERM/SIGHUP/SIGINT (restore first, then re-raise so the
exit status stays honest about the signal), and incremental key decoding —
escape sequences may split across reads, and a bare ESC resolves as a
keypress only after a pause.

## Verification

Contract behavior is table-tested (`go test -race ./...`), drift tags
included; the fetch pipeline's single-writer property and the claim's
test-and-set both have dedicated regression tests under the race detector.
What `go test` can't reach — signal-time terminal restoration, real picker
interaction — lives in the committed expect(1) harness (`make test-pty`,
`test/pty/`): SIGTERM must leave the terminal sane; arrows + enter must select
and write state; ESC must cancel writing nothing; the board must redraw its
own countdown from its own ticker and stay responsive under a held-down
refresh key (that the key cannot *become* traffic is the claim's property, and
is pinned in `go test`); resume must emit exactly its decision line on stdout,
write nothing on cancel,
survive SIGTERM inside the alternate screen, and delete only inside the
harness's fixture store — `HEADROOM_HOME` re-points the primary config dir
and session store, and exists so a `dd` test can never touch the real
machine's transcripts. Its two hard-won patterns are documented in
`test/pty/run.sh` — kill with `pkill -nx headroom`, never `-f`, and inspect
post-mortem terminal state via an sh wrapper running `stty` in the same
pty.

## Status

Personal-first: the defaults fit the author's machine, and `HEADROOM_*`
environment variables re-point each of them. The endpoint headroom reads is
undocumented and restricted by the vendor's consumer terms to first-party
clients; this repository exists as a personal, read-only instrument, not as
a distribution.
