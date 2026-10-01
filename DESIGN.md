# Design — why headroom is shaped this way

headroom is a read-only observer of systems it doesn't own: several Claude
Code logins on one machine, each keyed to its own config dir, and several
Codex CLI logins, each keyed to its own home. It answers "which account has
headroom left?" and lets the user act on the answer (the account board,
`accounts`: live and self-refreshing on a terminal with a page per vendor,
one frame off it, one row per account under `--compact`, a versioned document
under `--json`), answers "which session do I get back into, and on which
account?" (`sessions`, Claude Code only — see The session surface below),
turns the chosen account into a running session (`launch`/`resolve` — see
The launch surface), keeps one picture of each subscription's load when a
second home on the machine holds logins of the same subscriptions (see A
second home), and proves its own assumptions still hold (`check`).
This file records the mental model and the vendor contracts — the things the
code can't say about itself.

Most of this file is written about Claude Code, the vendor the model was
built on. The mechanisms it describes — discovery, `.current`, the claim
ledger, the three axes, launch preparation — are the same code for Codex;
A second vendor: Codex says where Codex differs and why.

## The system observed: the filesystem is the registry

There is no account list. Claude Code's default config dir (`~/.claude`)
plus every directory under the accounts root (default `~/.claude-accounts`,
one dir per extra login, named by its email) *is* the account set.
Everything derives from that tree:

- **Isolation — the load-bearing vendor fact, and its sharp edge.** Claude
  Code honors `CLAUDE_CONFIG_DIR` and keys its macOS Keychain item *per
  config dir*: service `Claude Code-credentials` for the default dir, with
  `-<sha256(dir)[0:8]>` appended for any other (verified against the binary,
  v2.1.220). Every dir is therefore an independent login, and all tokens
  coexist — but only for an **absolute** value. A relative one (reproduced
  twice, same binary) splits the session in two: config state goes to the
  cwd-relative dir while credentials come from the *default* Keychain item,
  so the child runs as the primary under a stray state dir — the 2026-08-03
  stale-wrapper incident's mechanism. headroom therefore only ever
  constructs absolute dirs: `config.Load` refuses relative `HEADROOM_*`
  overrides (and never rewrites an accepted spelling — the Keychain hash is
  keyed on it), and `launch.Extra` refuses a relative dir at construction.
  A second door into the same chimera, found while auditing the first:
  `CLAUDE_SECURESTORAGE_CONFIG_DIR` redirects the *credential* lookup
  independently of the config dir (verified 2.1.220 by experiment — with it
  set, `auth status` answers from the named dir's Keychain item while
  config state stays the config dir's). headroom never sets it and
  `launch.Target.Env` strips any inherited value, with the usual
  said-out-loud reporting on launch stderr and in `check`.
- **Lock debris is not an account.** Claude Code's config locking creates
  `<path>.lock` *directories* beside what it locks, and a crash strands
  them in the accounts root (observed 2026-08-10: `<email>.lock` beside the
  real dir). An adopted artifact is worse than clutter — the health probe's
  `claude auth status` initializes a skeleton `.claude.json` inside any dir
  it is pointed at, dressing the debris up as a hollow login. An email
  never ends in `.lock`, so discovery skips any such directory (an `.order`
  line included), and `check` names stranded ones instead of letting them
  vanish silently; deleting one is safe once no claude process runs.
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
- **Two one-line files beside the accounts.** `.current` says what a bare
  `x` does next — the name of the account it targets, or the word `auto`
  (see Automatic placement) — written atomically by headroom alone (the
  board's enter and `a`, `launch --remember`) and read strictly by headroom
  alone (the discovered set's `Bare`; see The launch surface for why corrupt
  state refuses rather than defaulting). `.order` (optional; one email per line, `#`
  comments) sets display order after the primary; unlisted accounts follow
  alphabetically — configuration a human edits. Both stay outside
  `state.json` on purpose; see The one file headroom writes. A third,
  `.ledger` (optional, Claude Code only), names the accounts root whose
  ledger this home spends against — see A second home.

Read-only means read-only *against Claude Code*: headroom never writes the
Keychain, never refreshes a token, and never touches vendor login or quota
state — Claude Code owns all of it. What it writes is its own: `.current`,
one `state.json` holding non-secret request timestamps, the usage
responses it fetched itself, session re-homes and the record of recent
launches, `launches.jsonl`, the launch log nothing routes by, and — only
when `accounts ledger` is asked to — `.ledger`. Three documented
exceptions, every one an explicit user command naming its object and refused
while a session is live or liveness is unverifiable: the session picker's `r`
appends one vendor-format `custom-title` record (exactly what the native
Ctrl+R rename writes) and `dd` deletes a transcript, both against the shared
store; and `accounts remove` deletes the removed account's Keychain item
(`creds.DeleteKeychainItem`, the one Keychain write in the codebase, reached
from no observation path). That last one is deliberate: removing an account
is not a read, the user's expectation of "remove" includes the login, and a
usable token left behind for a config dir that no longer exists is the worse
outcome. Nothing in it touches any *other* account's item — the service name
is derived from the removed dir's spelling alone.

### The account lifecycle

The set changes by two human acts, both engine commands so the topology
verifier's error messages name something that exists on every install:

- **`accounts add <email>`** (`accounts.Seed`) makes the dir, links its
  `projects/` to the canonical store (creating the store if the machine has
  none yet — an absent store on a fresh install forks no one's history; a
  store that is a symlink or a file refuses), optionally symlinks shared
  config, and finishes by running `VerifyTopology` on what it built — the
  same check launch applies, so seed and gate cannot disagree. Names are
  emails: one path element, `@` present, never `.lock`. Sharing is a
  caller choice, never a default: bare `--share-config` links a *whitelist*
  from the primary's dir (`accounts.SharedConfigEntries`, per vendor —
  for Claude Code settings, skills, commands, agents, hooks, plugins, …; for
  Codex `config.toml`, `AGENTS.md`, themes, skills, prompts, rules, plugins), and
  `--share-config=<dir>` links every entry of that dir (a config package,
  which holds config and nothing else). Whitelist and not blacklist because
  a config dir also holds what must stay per account: `history.jsonl` is
  session-ownership evidence, `sessions/` the live registry,
  `.credentials.json` a file-backed login, including when an SSH session
  cannot use the login Keychain. The list is
  vendor-perishable, but nothing load-bearing rides on it, which is why
  `check` does not verify it. Whatever the share mode, the entries that are
  never configuration (`accounts.PerDirEntries`: for Claude Code
  `.credentials.json`, `.claude.json`, `history.jsonl`, `sessions/` and the
  store link) are skipped and named: a config package that happens to hold
  a login must not make two dirs one login, and `check` fails on a dir
  whose login, history or registry is a link. Seeding never writes
  `.current` or `.order`.
  A dir that fails half-way is left in place and named: it is inert
  (discovery lists it, launch refuses it on topology) and deleting a
  directory to tidy up is not seeding's call.
- **`accounts remove [<email | name.lock>]`** names its object up front —
  the argument, or, bare on a terminal, a picker over the removable
  candidates (every extra plus any `.lock` debris; the primary is never
  offered) whose enter asks "remove X?" as one more key *inside the same
  raw session*, because a cooked-mode reply after the tui closed would be
  swallowed by its still-blocked stdin reader — there Enter or `y`
  confirms (the row was already chosen with Enter; a second Enter is
  re-affirmation, not a trap), `n` or any of the board's cancel keys
  (esc, `q`, ctrl-c, ctrl-d) cancels, other keys are ignored; a name nothing answers to
  prints the candidate list. Then it runs its refusals before anything
  irreversible: not the primary (Claude Code's own default dir has
  no entry in the accounts root); `accounts.CheckRemovable` — form, and the
  one refusal that guards data: a `projects/` that is a *real directory*
  holds sessions never migrated into the store, and `RemoveAll` would take
  them with the account; the liveness gate consumes `sessions.ReadRegistry`
  problems and `sessions.Inspect` evidence — an unreadable claim or registry
  directory refuses removal, as does any live or unverifiable session;
  then confirmation — `y/N` on a terminal, `--yes` off one, and refusal
  without either; a picker choice arrives
  already confirmed and takes `--yes`'s path — and the gate once more
  after the reply, since the prompt may have stayed open while a session
  started. Then the Keychain item, then the dir (including any file-backed
  `.credentials.json`; `os.RemoveAll` does not
  follow symlinks, so the `projects/` link goes and the store behind it
  stays — pinned by test), then the `.order` line. `.current` is never
  rewritten: a removed current account makes launch refuse until the board
  repicks, so corrupt-vs-chosen stays distinguishable. Transcripts survive
  (they are the machine's); the picker shows the dead owner as degraded
  until each is re-homed.

`claude auth status` is used for its answer only. It may or may not refresh a
token as a side effect; headroom neither relies on that nor invokes it hoping
for it. Building on an unpromised side effect would be the same class of
mistake as reading `expiresAt` as account health.

## The data source, and drift as a design input

The board calls the endpoint Claude Code's own `/usage` screen calls —
`GET https://api.anthropic.com/api/oauth/usage`. Every Claude Code account,
including the primary, resolves its credential from the Keychain item under
`ServiceName(Account.ConfigDir)` first, then from
`Account.Dir()/.credentials.json` if the Keychain read returns no blob. The
primary's `ConfigDir` is empty for the base service name, while `Dir()` is
the real `~/.claude` path. A readable Keychain item wins when both stores
hold credentials. File-backed logins occur in SSH sessions with a locked
login Keychain and on Linux. The board, `check` and its 401 re-check use this
same resolution rule and `creds.Parse`.

The selected blob supplies the Bearer token. The endpoint is undocumented.
`usage.ParseLimits` requires a `limits` array: an empty array means no
limits, while missing, null or malformed arrays mean an unreadable response.
Historical `five_hour` bodies are unsupported, including stored copies;
`check` reports them as vendor format drift. Every vendor contract here is
reverse-engineered and perishable:

- **One parser per vendor document type, and no duplicate parse paths.**
  `creds.Parse` (credential blob), `usage.ParseLimits` (usage response, live
  *and* cached — they are the same shape) and `auth.Parse` (auth status) are
  the only readers of Claude Code's data, as `codexauth` and the Codex usage
  parser are of Codex's; callers reach a usage parser only through
  `usage.Parse(vendor, body)`. Renderer and checker share them, so the
  checker cannot drift from what rendering actually needs.
- **Tolerant rendering, tagged degradation.** A malformed field degrades
  instead of dropping the account, and degrades visibly — a bad percent is a
  `?` bar with a drift marker, never a `0%` that reads like free headroom;
  accounts fail independently; and every parsed field carries a `tag.State`
  distinguishing *legitimately absent* (an untouched limit window, a
  credential field the vendor stopped sending) from *present but no longer
  parseable* (shape drift). `--json` carries the same tags outward, so machine
  consumers can't mistake drift for data either.
- **`check` separates failed assertions from missing evidence.** PASS (exit 0)
  means every assertion was tested and held; FAIL (exit 1) means one was
  contradicted; INCONCLUSIVE (exit 2) means an assertion could not be tested.
  Rate limiting, transport failure, token refresh races, lock contention and
  a valid newer state schema leave evidence incomplete. Corrupt headroom
  state is an own-state failure, distinguished from vendor drift in both the
  details and closing verdict. A configuration headroom refuses — a
  relative override, a `.ledger` naming nothing usable — is an own-state
  FAIL, never INCONCLUSIVE: every launch is refusing on it. A home that
  shares another's ledger has two files, and each is audited on its own
  terms: one written by a newer headroom is read and left alone without
  silencing a damaged section in the other. A claim failure
  marks the API untested; a completion failure leaves received vendor
  evidence available to check.
  FAIL takes precedence when a run also has inconclusive assertions. Every
  request uses the board's budget and storage operation. Its per-account
  `credential[...]` assertion names the selected source and requires a
  parseable blob; no credential is FAIL (not logged in).

Two deliberate tolerances beyond strict inherited behavior: numeric-epoch
`resets_at` values are accepted, and timestamps may carry offsets or
fractional seconds. Handled drift beats flagged drift.

## Three axes, because three things vary independently

`internal/accountstate` owns the facts shared by the command surfaces.
Health, observed usage and the latest request vary independently: a usable
account can have old observations and a refused refresh at the same time.

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
- **Attempt** — what happened to the newest request. Endpoint results and
  bookkeeping failures remain independent: "rate limited" and "state file
  unavailable" can both apply, while the account's health stays unchanged.

Two consequences worth stating. An access token aging out is an *attempt*
fact: the token lives ~8 hours, Claude Code refreshes it silently, and the
account is fine — only `refreshTokenExpiresAt` passing means a human must act.
And a failed refresh annotates an observation rather than replacing it, so the
board degrades to "58%, observed 22h ago, refresh rate-limited" instead of
to a bare error.

The board draws these axes in a block layout or, under `--compact`, a row
layout, and the axes decide the row's shape. The block spends a line per
fact: health in red, one bar per window, a provenance caption. The row
carries the label, one cell per limit window and a trailing caption, under
one rule — a row may not collapse what the block keeps apart:

- **The caption is every applicable clause joined, never one winner.** A
  logged-out account holding a fresh cache says "/login" *and* "via Claude
  Code's cache"; a mismatched dir is named, not only marked. The caption is
  what a narrow terminal cuts from the right, so its order is clip order:
  the clauses that change what the user does — health, the dir, why there
  are no figures, drift, `stale`, how the refresh went — before the ones
  that explain (how old, from where).
- **Columns are decoded identity, never heading prose.** They key on
  `kind`/`group`/`model`, the way machine consumers select rows, and run in
  the order that decides the choice: all models (the window that strands an
  account for days), then the 5h session, then the model-scoped weekly
  windows, which bind only for work on that one model. A row whose
  identity failed the contract forms no column and captions its account
  with drift.
- **A cell is a percent with its clock beside it, not beside it as an
  equal.** The percent carries the bar's severity colour and precedence
  (observation age does not change it; a percent that failed to parse is red); the
  time-to-reset sits after it dim, in its own aligned slot, because the
  clock is secondary to the number and belongs next to it rather than in a
  column of its own. That clock is one number with one decimal in both
  layouts — tenths of an hour under a day (2.1h), tenths of a day from a
  day up (4.7d): a weekly reset is waited for in days, and a pair of units
  reads slower than one figure. The bar's states stay distinct tokens: a valid reset, a
  legitimately absent one, one that failed to parse, a rolled-over window, a
  percent that is not a number.
- **Under width pressure the row gives way in cost order.** Heading
  surplus first (a heading wider than its cells ellipsizes), then the name
  toward its floor, and only then the caption's reserve — a fourth window on
  one account must not spend another account's warning on padding. The
  unhealthy account's name is red so even a caption clipped to nothing
  cannot leave green figures beside an account that cannot be used.
- **A layout is only a layout.** Both run the same rounds, honour the same
  claim and commit the same choice, and one renderer function produces the
  whole board — header and one line group per account — for both, so the
  picker and the one-shot print can neither compute a column nor disagree
  about one. Off a terminal, or on one that is not interactive, the print
  wraps rather than clips: there is no redraw to protect.

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
ledger, the responses, the session re-homes and the recent launches.

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
- **One store per accounts root, one ledger per set of homes.** Each vendor's
  `state.json` sits under its own accounts root beside its own `.current` and
  `.order`, so nothing in Claude Code's files is re-keyed by Codex existing,
  and the store a body was read from says which vendor's parser reads it. A
  file holds two halves with different owners: the *subscription ledger* —
  the request ledger, the responses, the recent launches and the homes that
  spend against it — is about quota, keyed by account identity; the
  *re-homes* are about one home's sessions, named by that home's account
  names. A home alone keeps both in its one file. A second home that holds
  logins of the same subscriptions keeps its re-homes in its own file and
  spends against the first home's ledger (see A second home); this is the
  one place the rule bends, and it bends for the ledger only. `config.Load` refuses two
  roots that name one location, judged by the filesystem and not the string:
  each root's nearest existing ancestor must be a different file, or the
  components still missing below them must differ — compared without case,
  since whether the filesystem folds a name cannot be known before it exists.
  A symlink anywhere above, and `Accounts` beside `accounts` on a case-folding
  volume, are both one location.
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
  its own provenance. The body is stored raw: the vendor's usage parser,
  reached through `usage.Parse`, stays its only reader, and no second row
  shape exists to drift from it.
- **Corruption degrades per section, and the two sections are not alike.** The
  ledger is disposable: unreadable bytes are set aside under a name nothing
  reads, and every account starts one full cooldown quiet, so the file
  self-heals rather than bricking — and never by guessing "eligible", which is
  the guess that generates traffic. Re-homes are human decisions: an
  unreadable sessions section refuses re-home changes and survives other
  writes verbatim, and records are validated whole, because a section that
  decodes *around* a hollow record reads as healthy while routing silently
  ignores it. Sections this binary cannot decode at all are carried through
  writes verbatim, and a document from a newer headroom is read but never
  rewritten.
- **Records are bounded by age, never by which accounts a caller could see.**
  Sweeping by absence needs a complete account registry, and no caller can
  promise one: an account's key comes from a vendor file Claude Code rewrites
  constantly, so a torn read moves the key and the sweep deletes the live
  cooldown and stored answer of an account sitting right there.
- **Launches are recorded beside the ledger, and are as disposable.** The
  `placements` section holds the launches of the last fifteen minutes — the
  load a new launch must count — the newest launch per subscription, which
  breaks ties, and the newest launch per home, which answers `--last`. Load
  is keyed like the ledger, by the account's identity, because two dirs on
  one account spend one quota; `--last` is keyed by home, because an account
  name is one home's — two homes name their dirs by the same emails. Every
  launch carries the home that made it, and launches recorded before homes
  shared a ledger read as the ledger's own home's. A
  section that will not decode is set aside and the record starts empty: that
  costs a few launches their view of each other and nothing else. A binary
  from before the section existed carries it through its writes untouched.
- **`.current` and `.order` stay out of it.** `.current` is one human-legible
  routing fact with its own failure policy — corrupt refuses the launch,
  visibly — while this document's ledger section is disposable and
  self-heals by quarantine; folding the two together would put a routing
  decision under the ledger's corruption policy or vice versa. `.order` is
  configuration a human edits, comments included.

The upgrade path imports `.owners` and `.throttle` once. Reads preview their
facts; a locked mutation checkpoints all imported re-homes and cooldowns
together before attempting archival as `.imported` recovery copies. A failed
write leaves the inputs available for retry. A crash or failed archive after
commit leaves the originals inert: the checkpoint ends legacy reads, even
when the initial import found no legacy files.

Readable re-homes remain available independently of request-ledger damage.
Unreadable legacy re-homes block checkpointing and mutations until repaired,
as does a current sessions section into which legacy owners cannot safely
be merged. This protects human decisions from retirement before preservation.
A damaged legacy request ledger instead imports one maximum cooldown.

The imported cooldown is a one-time global quiet period, capped at 16 minutes
when checkpointed. It protects accounts outside the first claim, including
identities now accessed through a different directory. Other accounts may
wait too; ordinary per-account scheduling resumes after the deadline.
Concurrent writers from retired binaries are outside this upgrade contract.

`internal/refresh` owns the live-request operation for the board and checker:
credential and identity eligibility, claims, fetches, interpretation and
completion. Received observations retain their arrival time; a persistence
failure annotates the endpoint verdict. Rendering presents both, while JSON
keeps the verdict in `attempt` and bookkeeping failures in document-level
`problems`. The checker reports each independently and samples credentials
once after a 401 to distinguish a refresh race from unchanged or unreadable
evidence. The board consumes the endpoint result without that diagnostic read.

Two policies fall out of the budget. The spacing is a per-vendor value, and
every deadline the ledger computes takes it as a floor — the clamps that cap
a deadline at the maximum cooldown included, so a vendor whose spacing sits
above that cap is never handed an earlier retry by one. Figures are labelled
stale only once they are older than the request spacing — inside that window no newer answer is
obtainable, so nagging about age would be nagging about something nobody can
act on. And the board's refresh cadence *is* eligibility: there is no interval
to configure, because asking sooner is refused and asking later says less
than it could. Presence bounds it: a board left open on a spare tab stops
asking after a few minutes without a keypress, since a surface that polls for
hours with nobody looking is the background daemon this design declines to
have, wearing a TUI.

The interactive board is one page per present vendor. A page owns its list,
selection, scroll position, schedule and its in-flight round, so a result
lands on the page that started the round and never on whichever page is
visible when it arrives. Only the visible page starts rounds: a hidden page
keeps its figures and their age, its deadline never wakes the loop, and one
that passed while hidden is simply due at the next tick. Tab switches pages,
the tab bar ranks with the header when the terminal is short, and enter
records the visible page's vendor's `.current` and nothing else. With one
vendor there is no tab bar and no heading. Off a terminal each present
vendor runs one round, side by side, and the pages print in vendor order
under a heading each.

`r` means *ask now*, and the claim — never the board's loop — answers. A
round owns its account list and result channel until the channel has closed
and drained. Its local half re-reads everything free (health,
discovery, credentials, `.current`, the store's replay of what another
surface may have bought seconds ago); its budget half is the claim plus
whatever fetches it permits. `r` runs both immediately; an account still
inside its quiet period comes back annotated with when it can next be asked,
never silently re-armed into the cadence — pre-deciding eligibility in the
loop was the duplicated-eligibility bug the claim exists to close. It never
buys an exemption, because an override would spend against a bucket where a
refusal can itself cost more than it buys. The one veto is a 30-second floor
between rounds — spawn hygiene for the local half's per-account auth probe
under a held key, never budget arithmetic — inside which the press queues a
round that fires the moment the floor passes, unattended included, because
it was asked for. The status line briefly acknowledges the completed round:
"all current" for successful responses, including an empty limits array;
"usage in …" when an answer is still deferred. Eligibility for a future
request belongs to scheduling and does not make a successful answer deferred.

## The limits surface: reading without spending

Machine consumers exist — a statusline refresher wanting one account's
weekly numbers a few times a minute — and the board's `--json` is the wrong
instrument for them twice over: it probes health for every account
(`claude auth status` at ~170ms each is the dominant term of its ~300ms
warm), and it may spend every account's request budget to answer about one.
`headroom limits [--vendor <v>] [--account <name>]` is the read that fits:
the same versioned document, assembled from disk alone — discovery, `.current`, and
the newest stored observation through `accountstate.Read`
(headroom's own store and Claude Code's cache, newest wins). No health
probe, no Keychain read, no claim, no request: it is not a second door onto
the endpoint because it never touches the door at all, and it answers in
about 10ms. Nor a process sample: its accounts carry `load: null`, since
liveness costs a `ps` per session and busy is a fact about now. Refreshing
what it reads stays the fetching surfaces' job — the board, `--json`,
`check` — all behind the one claim.

Two honesty rules keep the skipped work visible. Health reports `unprobed`,
a statement about this surface and never about the account, distinct from
`unknown` (the question was asked and had no answer) — a read that skipped
the question must not let its silence pass for an answer. And the attempt
axis stays `none`, with the ledger's next-eligible instant riding along as
advice: a consumer deciding when to trigger a real fetch can respect the
budget that fetch will be claimed against, while the claim alone still
authorizes. The same honesty covers headroom's own file: problems reading
`state.json` surface in the document (the `--json` board's too), because
without them an unreadable store and a first run both serialize as
`usage: null` — and the second is a silence that lies.

The document is one JSON object whatever the vendors. `accounts[]` is one flat
list and every account and problem carries its `vendor`, because the same
email can be an account of both; `current` is an object keyed by vendor,
holding the vendors in the document. `--vendor` restricts all of it to one
vendor, and `--account` filters by name inside the selected vendors, so an
email both vendors know answers twice.

Rows are selectable by decoded identity, never by prose. Each limit carries
the vendor's own vocabulary verbatim — `kind` (observed: `session`,
`weekly_all`, `weekly_scoped`), `group` (`session`, `weekly`), and the
scoped model's display name (`scope.model.id` exists but has only ever been
observed null; it becomes the better selector the day it is populated) —
beside the rendered label, which is derived from those fields and nothing
else, so the two cannot disagree. A consumer that matches the label is
matching prose that changes the day a model is renamed; `kind` equality is
the contract. Identity degrades like every other field: a row whose
identity fields drifted, that lacks the `kind` consumers select by, that is
scoped without saying to what, or whose known kind contradicts its scope or
group (a session or all-models row carrying a model, a weekly kind under
another group) is tagged
`identity_state: "bad"` and counts as drift, so
`check` fails before a consumer quietly stops matching — the scoped-weekly
case matters most, because the model-scoped weekly is routinely the binding
limit while the all-models figure sits far lower, and a scoped row
mislabeled as all-models would read calm at the exact moment work stops.

## The session surface

Session transcripts are machine-global — every account's `projects/`
symlinks to `~/.claude/projects` (`accounts add` creates that topology) — so
one picker over that tree sees every conversation regardless of account.
`headroom sessions` is that picker. Its vendor contracts, all reverse-
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
  `enter` resumes on the owner and writes no re-home — the launch itself
  becomes vendor evidence. The override key re-homes: it records the one
  fact the vendor never will ("the user pointed this session here before
  prompting") and is superseded by any newer evidence. An automatic launch
  that places a session it was given by id writes the same record, for the
  same reason: a print-mode session leaves no prompt history at all, so
  without it each later turn would be placed afresh. The sweep that retires
  re-homes of deleted transcripts leaves a record younger than ten minutes
  alone — a first turn's id is known before the vendor has written a line.
  Degradation is visible: no evidence, a re-home to a deleted account, or a
  same-instant conflict each fall back to the current account under their
  own tag. Under auto mode there is no current account, and each of those
  asks the placement rule instead; the override then moves the session to
  the least-loaded of the *other* accounts, since moving means somewhere
  other than where it is.
- **Retention is one setting, read per account.** Claude Code's cleanup
  sweep deletes transcripts older than the `cleanupPeriodDays` in its config
  dir's `settings.json` (30 when unset), and through the store link every
  account's sweep prunes the one shared store — so the store keeps only the
  shortest period any sharing account names. `check` reads each sharing
  account's value and fails when they disagree; an unreadable or rejected
  value (below 1) is untested, since Claude Code pauses its sweep then too.
  The value itself is the user's policy, printed on the ok line, never
  judged. Proving cross-account resume end to end would take a real request
  on two accounts and leave a transcript in the store, so `check` does not:
  the topology line is the static proof, and every resume from the picker
  exercises the rest.
- **Liveness is pid + start instant, and it gates the mutations.** Each
  account dir's `sessions/<pid>.json` registers a running session. A claim
  counts only when the pid is alive *and* its kernel start time matches the
  registry's `startedAt` within tolerance — pids recycle. (Epoch ms, not
  the `procStart` string: that one is UTC-rendered while `ps` speaks local
  time, so string equality fails everywhere but UTC.) The same record
  carries `status` — `busy` or `idle` on 2.1.286, `shell` seen once — which
  the reader passes through verbatim; only `busy` is given a meaning, by
  placement, and no other word is thereby "idle". Live and
  unverifiable sessions refuse `dd` and `r`; deleting an open transcript
  loses the conversation to an unlinked inode. Registry decoding returns
  claims plus read problems. Each PID is sampled once per collection, so
  ownership and liveness agree. Proven absence or a recycled PID clears a
  claim; failed inspection stays unknown. An unreadable registry directory
  leaves session liveness unknown. A damaged record with a session ID guards
  that session; a nameless damaged record blocks account removal and appears
  in `check`, without attributing it to unrelated transcripts.
- **headroom decides, enters, and becomes the session; the shell may cd
  after.** The TUI runs on `/dev/tty` (alternate screen, restored in the
  same signal path as raw mode); enter re-checks liveness and the dir,
  resolves the account through the same validated `launch.Target` the
  launch surface uses, verifies topology and the relocated-home rule,
  resolves the `claude` executable *before* changing directory (a relative
  PATH entry in the project must never supply the binary), then restores
  the terminal, chdirs to the verified cwd, repairs `PWD` in the child
  environment, and execs `claude --resume <id>` with the caller's `--`
  flags ahead of the picker's own. No routing decision leaves the process.
  Requires real stdin/stdout terminals — a captured stdout would hand
  claude a pipe — and refuses `--resume`/`--continue` as pass-through
  flags: the picker chooses the session. The one thing only the parent
  shell can do — make its own `cd` stick once the session ends — is served
  by `--cd-file <abs path>`: created (or truncated) at flag parse, written
  with the entered dir only after a successful chdir, `Sync`ed before the
  exec. A newline-containing cwd is refused only when this file is requested.
  The contract is exactly two states: empty means "no launch was
  committed — do not cd" (cancel and every refusal), non-empty means "this
  dir was entered" (regardless of how claude then exited). Nothing
  routing-relevant is representable in it, and deleting the flag changes
  no routing — it is advisory by construction. A write failure is one
  stderr line, never a refusal. Deliberately without `--remember`, which
  the exec makes tempting: `.current` means "where new sessions go", and
  this picker leaves it untouched. The picker itself does no network I/O
  and spends none of the usage budget; a resume is a launch like any other
  (see Automatic placement), so one the rule places leaves the same
  detached refresh behind it.
- **`resume` is a tombstone, permanently.** The predecessor printed a
  `dir\tid\taccount` line for the wrapper to recombine, and a shell
  function loaded before the account-name protocol misread the third field
  as a config-dir path — the 2026-08-03 incident. The verb changed with
  the contract (see The launch surface on why a renamed verb, not a
  version tag), and the old spelling now exits 2 with an actionable
  stale-shell message and nothing on stdout, `--json` included: one name,
  one meaning, across binary generations. The arm is kept indefinitely —
  "no shell still runs the old wrapper" is unobservable, and the cost is a
  case and a string.

## The launch surface

Launch routing is headroom's own semantics, not the shell's. The incident
that forced this: a tmux server started inside a Claude Code session carries
that session's `CLAUDE_CONFIG_DIR` in its global environment, and a wrapper
that set the variable for extra accounts but left the ambient environment
alone for the primary silently routed every "primary" launch to whatever
account that session ran on — while the board truthfully reported the
primary as pinned. The invariant, and the reason `internal/launch` exists:

- **The child environment is a total function of the validated decision.**
  The variables are one policy table with a row per vendor
  (`config.EnvPolicy`), applied by one constructor; a target strips only its
  own vendor's variables. For Claude Code, every inherited
  `CLAUDE_CONFIG_DIR` is stripped; exactly one is set for a
  non-primary account; the primary is selected by the variable being
  *absent* — the only behavior verified against the binary (present-but-
  empty is unverified territory and is never produced) — except in a home
  that is not the user's login home, whose primary is spelled out like an
  extra (see A second home). The decision crosses
  the seam only as a validated target ("primary plus a dir" and "extra
  without one" are unrepresentable), and auth's per-account probes build
  their environments through the same constructor, so a polluted shell can
  neither re-route a launch nor make every health answer come from one
  login.
- **`headroom launch [--auto | --last | --account <name>] [--remember]
  [--dry-run] [-- args]`** decides the account (bare means the recorded
  choice: the pinned account, or under auto the rule's — see Automatic
  placement), optionally
  records it as where bare `x` goes next — before the exec, and a failed
  record refuses the launch — then execs `claude`, preserving stdio,
  signals and exit status by replacement rather than proxying. `--remember`
  is an explicit ask, never a launcher default: the wrappers' generated
  `x-<name>` launchers deliberately omit it, because a named launch means
  "this session, that account" — scoped — and a pin riding along as its
  side effect let a two-minute hop to another account silently retarget
  every later bare `x`. In ordinary use only the board's enter moves
  `.current`; the flag remains the scriptable spelling of that decision.
  Both launch entry points call `launch.Prepare` before recording a choice:
  routing, relocated-primary refusal (a primary selected by absence under a
  re-pointed `HEADROOM_HOME`), shared topology, executable path and child
  environment are resolved together. A failure at that stage leaves
  the choice untouched. If process replacement fails after persistence, the
  remembered account or re-home remains recorded and the error says so.
- **The shared-sessions topology is headroom's invariant, verified at
  launch.** The session surface's whole model — one picker over one
  machine-global store — holds only while every extra account's `projects/`
  is a symlink resolving (by inode, not path spelling) to the canonical
  store; a violation forks session history silently. So `launch` verifies it
  before every extra-account exec and refuses with the exact end state
  required, `check` asserts it per account through the same verifier
  (`accounts.VerifyTopology` — one function, so the gate and the report
  cannot disagree), and creation belongs to `accounts add`: a launch that
  quietly repaired topology would hide exactly the state the refusal exists
  to surface.
  **`headroom resolve [<name>]`** prints
  `canonical-name<TAB>config-dir<TAB>kind` (kind: `primary`|`extra`) for
  shell preflight: the dir so personal checks can run against it, the kind
  so *which* preflight applies is headroom's classification — a wrapper
  that re-derives it by prefix-matching its own idea of the accounts root
  silently skips its checks whenever a `HEADROOM_*` override moves the real
  one. Resolve is advice, not a capability: launch revalidates whatever
  comes back.
- **Selection fails closed.** An absent `.current` is the documented
  fresh-start default (the primary, pinned); the reserved word is auto mode;
  an empty, unreadable one, or one naming a deleted account, refuses with an
  actionable message — on the
  launch, in `check` (an own-state FAIL), and as an unset `← current` marker on
  the board. The old shell fallback turned all of those into "launch the
  primary with permissions bypassed", which made corruption
  indistinguishable from a choice; no surface may mint a primary decision
  from corrupt routing state, resume's attribution fallback included (owner
  → valid current → refuse — never a final fall to primary).
- **A neutralized inherited value is visible when it cannot be explained,
  never silently obeyed.** Every managed launch strips the ambient variable
  regardless; what varies is the reporting. A value naming a discovered
  extra account's dir is what every shell *inside* a managed session
  inherits — this machine's ordinary environment all day — and a notice
  that fires on the ordinary case is noise, so the board's note and the
  launch's notice stay quiet for it (the discovered set's `KnownDir`,
  one classifier for both, beside the target's own conflict classifier so
  diagnostics and the environment built cannot disagree). What stays loud, because tools
  *outside* the managed path still obey the variable: a relative value
  (the stale-wrapper incident signature — an own-state FAIL in `check`), a
  value naming nothing discovered, and the primary dir spelled explicitly
  (present-but-primary is not the verified absent state). `check` reports
  every present value either way — full detail is what a diagnostic
  command is for.

headroom *advertises* only guaranteed launcher identities: the account's
full name — its email per account dir, the primary's login local part (or
`HEADROOM_PRIMARY_NAME`) — spelled through `HEADROOM_LAUNCHER_FORMAT`
(`accounts.Launcher`), which defaults to `headroom launch --account <name>`
because that command exists on every install; a shell integration sets the
format to its own names (`x-%s`) so the board promises the spelling that
resolves there. Short local-part aliases are shell convenience, invisible
to headroom — the old shared naming policy ("keep the two rule copies in
sync") was a cross-repo contract that could drift. Wrappers own what is
personal: the share source at seeding, flags, aliases — and they never touch
`CLAUDE_CONFIG_DIR`, never parse `.current`, and hold no verification a
launch depends on (topology verification moved into the binary for exactly
that reason). When headroom is missing or refuses, the wrapper stops loudly
rather than degrading to bare `claude`, which in a polluted shell is
exactly the misroute the managed path exists to prevent; the unmanaged
escape hatch is deliberately explicit (`env -u CLAUDE_CONFIG_DIR claude`,
or the variable set by hand).

The division follows from the two layers' lifetime models, and the rule is
worth stating because both wrong-account incidents violated it: **the shell
layer is init-frozen — a zsh function loaded at shell start lives for
weeks, and no code shipped today reaches it — while the binary is
re-resolved from PATH at every invocation.** Anything whose staleness can
misroute, mutate, or misparse therefore lives on the binary side; the
frozen side gets names, personal flags, and the one thing only a parent
shell can do (make a `cd` stick). The same asymmetry decides how protocols
may evolve: a version tag inside a line only helps a reader new enough to
check it, so a breaking change to a wrapper-facing surface ships as a
*renamed verb* — argv is the one channel where the binary can observe a
stale caller and refuse it, actionably. (An unchanged surface may keep an
old spelling — `select` still aliases the board — but a surface whose
contract changed must never reuse its name.)

## Automatic placement

A person with several subscriptions spreads sessions by hand: a named launcher
per session, or a trip to the board. Under auto mode a bare launch chooses the
account itself. The mode lives where the pin lives — `.current` holds an
account name or the word `auto` — because it is the same routing fact, and one
strict read with one failure policy cannot disagree with itself. It is not a
flag the shell wrapper passes: shell functions are frozen at shell init, and
panes would disagree about what `x` does for weeks. The costs are a reserved
word (an account so named makes the file ambiguous, and ambiguity refuses) and
that turning auto on forgets the pin.

- **Placement is launch-time and nothing else.** A session runs where it was
  started; no request is proxied and no running session is moved. The promise
  is that new sessions spread. A session can still exhaust the account it was
  placed on.
- **Each limit does one job.** `internal/placement` is one pure function. The
  session window — the vendor's shortest limit, five hours for Claude Code —
  ranks accounts as *load*: its usage in steps of ten points, plus one step
  per busy session and one per launch of the last fifteen minutes — on that
  subscription, in any home that shares the ledger. Any limit
  at 80% or above sets an account aside while another has room. Weekly room
  breaks ties, then the least recently placed account. The first draft of
  this rule folded both windows into one figure ("the most room on the
  tightest limit"), and with session usage near zero the weekly figure
  absorbed every increment a launch added: four launches in a row went to one
  account. Counting usage in steps, and busy sessions in the same unit, keeps
  a one-point difference from outranking forty points of weekly room, and an
  account at 9% with five busy sessions from beating one at 11% with none.
  The constants are policy, not measurement, and the rule's name travels in
  every log line.
- **Weekly room matters as much as the five-hour window.** Over thirty days of
  transcripts on the author's machine, messages about the model-scoped weekly
  limit appeared on four days and messages about the five-hour limit on one.
  That counts messages, not lost work, and it is enough to say the weekly
  limit is not the rare case. So a weekly limit can take an account out of
  the running, and decides every tie — but it never hides a five-hour
  difference between two others.
- **A figure counts only for the window it describes.** An observation older
  than fifteen minutes is stale and its rows are lower bounds; a row whose
  reset has passed counts as zero, because that window has ended. The board
  shows the same row as unknown, and both are right: a display must not print
  a low percent for a window that is over, while a placement may use it as a
  bound because it says so and counts its own launch against it.
- **An account that cannot be asked is tried, not avoided.** Only the vendor
  refreshes an access token, so an account nothing has used for about eight
  hours cannot be asked — and those are the idle accounts, the ones new
  sessions should go to. Its last figures order it like any other account,
  the launch line says how old they are, and its own placement keeps the next
  launch from following on the same unverified figures. If the account was in
  fact exhausted from another machine, that one session meets the limit at its
  first prompt; starting it refreshes the token, and the next launch can ask.
  Rationing unknown accounts instead would have sent five launches in six to
  the two accounts already in use.
- **Exclusion needs positive evidence.** An automatic choice leaves an account
  out when its identity document parsed and names nobody, its refresh token is
  demonstrably expired, the vendor says it is blocked, or it cannot be
  launched at all. A credential that could not be read excludes nothing: with
  a locked Keychain every account reads that way, and in auto mode every
  usable account is a correct answer. For the same reason missing bookkeeping
  never refuses an ordinary launch: a busy lock or a newer schema costs the
  launch its record and a line on stderr, and a placements section that does
  not decode is set aside and begun again by the launch's own placement,
  like the request ledger. Corrupt routing state still refuses, since an
  unreadable `.current` says nothing about what the person chose.
- **The board's mark and the launch are one judgment.** `← next` and a launch
  build their candidates with one function (`buildCandidates`), from the same
  credential reader and the same exclusions, so the row the board marks is
  the row a launch would take from those figures. A builder of the board's
  own can share the rule and still disagree on who is eligible, and then the
  mark names an account a launch refuses. The mark stays advice — a launch
  reads the disk again — but it is never a second opinion. The same counting
  gives every row its load on the board and in `--json` (schema 7): busy
  sessions and recent launches, with another home's share named, so the
  owner sees what the other home is running on each subscription.
- **Choosing and recording are one store operation.** `state.Place` reads the
  recent launches, calls the rule and records the result inside one locked
  section, which is `Claim`'s shape applied to launches: two launches started
  together cannot both see an account as empty. The rule is imported and
  called by the store, so no caller hands a function into the lock. Every
  launch goes through it — a pinned or named one with its account forced —
  because a session started by name is load an automatic one must count.
- **A launch is one operation, whoever asks for it.** `headroom launch` and
  the session picker each build an intent and hand it to `placeLaunch`, which
  places, prepares, logs and announces. What the launch means for a session
  travels into `state.Place` with it, so the load and the session's re-home
  are one write: both land or neither does. The picker's `x` is the one
  launch that depends on its bookkeeping — the re-home is what routes the
  session's next turn — so it is refused when the re-home cannot be written,
  and a refusal leaves no placement and no log line. A surface that ran
  those steps itself could refuse after recording, and a move that never
  happened would count as load and answer `--last`. What a launch owes the
  next one is decided there as well: a launch the rule placed leaves the
  refresh behind it, and a launch that records a session sweeps the re-homes
  whose transcripts are gone. Left to the surfaces, a resume the picker
  places would refresh nothing, and an agent that names a new session id on
  every run would grow the file until somebody pressed `x`. A surface keeps
  its terminal, its working directory and the exec.
- **Every launch says where it went, and what its figures rest on.** One line
  on stderr before the vendor starts, on every path — automatic, pinned,
  named, `--last`, the picker — because a line that appears on some paths
  teaches nothing about the others. A figure from an old observation is
  printed as a bound (`≥12%`), a window whose reset has passed as
  `window ended`, a percent that does not parse as `?%`: the rule counts
  each of those differently from a measured figure, and the line must not
  print them as one.
- **A launch counts for a fixed span.** Fifteen minutes, whether or not its
  process is still alive and whatever was observed since: a short job that has
  exited still spent what the figures have not caught up with, and an
  observation taken a second after a launch reflects none of it. Once the
  vendor's registry reports that same process as busy it is counted once.
- **The launch asks no network.** It reads discovery, the stored observations,
  the session registry and credentials, in a few tens of milliseconds. Figures
  are kept fresh for the *next* launch by a detached `headroom refresh` it
  leaves behind: the ordinary round, through the claim, recorded by a process
  that lives to record it. Waiting on the endpoint inside the launch would
  have needed a deadline across the claim, the fetch and the completion, and
  an answer still in flight at the exec would have been a spent request
  nobody recorded. The refresh lives inside whatever launched it — a
  supervisor that tears an automated job down counts it as the job's — so it
  is bounded (twelve seconds for the claim and the fetch, the completion
  after) and stops cleanly on SIGTERM, SIGINT or SIGHUP whatever it is
  doing: a `security` read in progress is killed, in-flight requests
  are abandoned and completed as what they are, a failure that says nothing
  about the budget, an answer that already arrived is kept, and nothing is
  claimed once the signal has come. Killed outright it leaves a claim that
  expires at its spacing — a claim is a reservation with a deadline that no
  reader waits on — and the lock dies with the process.
- **A named session keeps its account.** When the arguments carry an explicit
  id — `--resume <uuid>`, `--session-id <uuid>` — the launch follows the
  session's owner while that account is not near a limit, so its prompt cache
  and its checkpoints stay usable. `--continue` and a bare `--resume` name a
  session only the vendor can identify; headroom infers nothing and records
  nothing about them, because a guessed id written down as routing would
  misattribute a session for as long as the record lived. Three facts stay
  apart: where a session is routed (the re-home), which account drove it (the
  registry and prompt history), and how much load is on its way (the
  placements).
- **The log explains; it is never an input.** `launches.jsonl` holds one line
  per launch with every candidate as counted. No routing code reads it, it
  has its own lock, and it is bounded by age once it grows. Appenders hold
  that lock shared and the pruner holds it exclusively, since a prune
  rewrites the file and a record appended to the old one meanwhile would be
  lost; an append waits a quarter of a second for a prune and then gives the
  line up, and one that follows a torn line starts on a new one. Replaying
  another rule over it shows what that rule would have decided on the
  recorded inputs, not what the usage would then have been.

Codex follows the same mode through its own `.current`, with what it has:
the shortest window of its main rate limit as the session window, its
allowance as the block, and launches as the only load. It has no registry to
read and no session ownership, so `resume` is placed like a new session.

## A second home

The machine can carry a second consumer of Claude Code under the same OS user:
an automated pipeline with its own `HOME`, started by LaunchDaemons with no
terminal and no login Keychain, running many headless sessions through a
launcher prefix. It holds its own logins of the same subscriptions the owner
holds, in its own dirs, with its own settings, skills and session store —
and nothing of the owner's may reach its sessions, nor the reverse. What the
two must share is the picture of each subscription: its request budget, its
figures and its load, because the quota is the subscription's and both homes
spend it.

- **The unit is the subscription.** Every fact about quota is already keyed
  by the account's identity — the UUID in `.claude.json` — because two dirs on
  one account spend one budget. Two homes' dirs on one account are the same
  case one level up, so nothing about the key changes: the second home spends
  against the first home's ledger. A claim from either home is the claim the
  other is denied by (one request per subscription per spacing, whoever asks
  first, with that home's own token), a response either bought is the figure
  both place on, and a launch from either is load on the subscription for
  both.
- **A second accounts root that spends against the first's ledger.** The
  second home is an ordinary home — its `HOME`, its primary, its accounts root,
  its `.current`, `.order`, launch log and session store — whose accounts root
  holds `.ledger`, one line naming the first home's accounts root. `headroom
  accounts ledger <root>` writes it by the rule `config.Load` reads it with:
  absolute, an existing directory, not the Codex root, and a ledger of its
  own — a root whose `.ledger` names somewhere else is a pointer, and roots
  naming each other would each spend against the other's file while both
  read as shared. Naming one's own root removes the file. A file that breaks
  the rule refuses every command, the way a relative override does: a home
  that meant to share and quietly kept its own ledger would ask every
  subscription twice and pile launches onto accounts the other home is
  filling, silently. It is a file and not a variable because the second
  home's processes start from several places (plists, ssh, sessions
  dispatching sessions), and a variable lost on one path forks the ledger on
  that path alone; a file beside `.current` travels with `HOME`. Two other shapes lose on the re-homes: a profile kept
  in the owner's configuration is unreachable from a process that has only
  its own `HOME`, and one shared file holding both homes' re-homes would mix
  records named by account names that both homes spell alike — and each
  home's sweep sees only its own store.
- **What is shared and what is not.** The ledger root's `state.json` holds the
  request ledger, the stored responses, the placements and the member homes.
  Each home's own `state.json` holds its re-homes, and its launch log, its
  registries and its prompt history are its own. A session picker lists its
  own store, routes by its own re-homes and sweeps only those; ownership is
  never inferred from another home's registry. `--last` means this home's last
  launch.
- **Choosing and recording are still one locked operation.** `state.Place`
  takes the ledger's lock and then the home's, always in that order — it is
  the only operation that holds two, so nothing can wait on it in reverse —
  and decides with both held. The two writes are two atomic renames, ordered
  so that a refusal leaves nothing: a move that depends on its re-home (the
  picker's `x`) writes the re-home first and refuses if it fails; any other
  launch writes its placement first. Each partial outcome is one the design
  already tolerates and says out loud — load without routing is an unreadable
  sessions section's note, routing without load a busy lock's.
- **Each home registers where it lives.** A request or a launch records its
  home in the ledger — accounts root, home dir, how its primary is selected —
  written only when that changes or is a day old, so a claim that permits
  nothing stays a read; `accounts ledger` registers at once, so a home that
  has just joined is counted before its first launch. From that record
  another home discovers its account dirs and reads their live-session
  registries, one process sample per pid across both homes, and counts each
  busy session against the subscription its dir is logged into. Those dirs
  are never candidates, never owners and never swept; a dir whose identity
  cannot be read is not attributed at all, since its name is its home's to
  choose. A registration says where a home lives, never that it still
  shares: that is what the home's own `.ledger` says now
  (`accounts.OtherHomes`, the one resolver placement and `check` both use),
  so a home that leaves stops counting at once, and its record drops out
  when nobody has seen it for the retention period.
- **A second home spells its primary out.** When `HOME` is not the user's
  login home (from the user database, never from `HOME` itself), the primary
  is launched, probed and read with `CLAUDE_CONFIG_DIR` set to its dir, like an
  extra: Claude Code then keeps that dir's `.claude.json` inside it and keys
  its credential on the dir — which is exactly how such a home has run all
  along. Selection by absence there would read `$HOME/.claude.json`, another
  file, and key the credential on the base Keychain item, which is per OS user
  and belongs to the login home's primary. Observed on the author's laptop
  (macOS 26, Darwin 25.5), not yet on the machine the second home runs on:
  with `HOME` overridden, `security` searches no login keychain — only the
  System keychain — so that item is out of reach today and logins made under
  the second home's `HOME` land in `.credentials.json`; the spelling does not
  rest on that. The child environment is still a function of the decision:
  for that primary the decision is a dir. Its own sessions inherit that dir,
  so it is a known value there, not a notice.
- **Load says whose it is.** Every surface that shows load names another
  home's share by the home dir's name: the launch line (`load 3 (2 from
  steward-home)`), `--dry-run`'s note, the launch log's per-candidate
  `homes`, the board's `sessions:` clause, and `load.homes` in `--json`.
- **Bounds.** Only Claude Code homes share a ledger: Codex placement for a
  second home is not built, and Codex has no registry to read. Both homes
  run a binary that knows homes: one that does not, rewriting the ledger,
  carries the member homes through as an unknown section but drops the
  per-home tags inside the placements, which costs `--last` and the
  attribution of those launches, never their load. A dir without an identity
  keys by its name (`dir:<name>`), which both homes may spell alike; such a
  dir has no login, so it has no budget to confuse.

## A second vendor: Codex

The vendor set is closed at `claude` and `codex`. What differs between them
enters in exactly two ways. What is only a path, a name or a number is data
on a per-vendor **scope** that `config.Load` resolves — primary dir, accounts
root, the canonical session store and the name of the per-account link to it,
binary, usage URL, request spacing, launcher format, environment policy,
presence — and an account carries the scope it was discovered under, so no
operation takes a scope beside an account and the wrong pairing cannot be
written. What is a *vendor document* is read at three dispatch points and no
others: identity at discovery, access (health and the request candidate) at
preparation, and the usage body through `usage.Parse(vendor, body)`, which
live interpretation, replay and `check` all share. What a vendor supports as a
command is decided in the command that owns it, in plain sight: the Codex
removal refusal, Claude Code-only sessions, each vendor's group of checks.

The alternatives this beats: a data-only descriptor cannot say how identity,
health and credentials are read, so those rules leak into callers behind
flags; an interface with a method per stage, or a parallel Codex pipeline
glued at the app layer, gives claims, freshness, launch ordering and topology
two owners — exactly the mechanisms whose single spelling this design
protects.

Both scopes always resolve; presence (`~/.codex` or the Codex accounts root
exists) only decides whether the board, `--json` and `check` include Codex
unasked. A command naming an absent vendor fails with that reason, except
`accounts add`, which is how the first Codex directory comes to exist. Claude
Code is always present.

### Codex observed

Verified on codex-cli 0.155.0 unless marked; as perishable as every contract
here.

- **Isolation.** `CODEX_HOME` selects the whole state dir (default
  `~/.codex`); logging in under a second home leaves the first's login
  byte-identical. A missing path is a hard error and a relative one is
  canonicalized against the cwd, so headroom only ever hands it an absolute
  dir. The Codex desktop app shares `~/.codex`; headroom never writes that
  home's `auth.json`.
- **The login is a plain file.** `$CODEX_HOME/auth.json` holds `auth_mode`,
  `tokens{id_token, access_token, refresh_token, account_id}` and
  `last_refresh`. The id token carries `email` and, under
  `https://api.openai.com/auth`, `chatgpt_plan_type`, `chatgpt_account_id`
  and `chatgpt_user_id`. It lives one hour and is routinely days expired on a
  working account, so its expiry is never read. The access token lives about
  ten days; only its `exp` is read. `internal/codexauth` is the one reader —
  JWT payloads decoded, signatures not verified, because the file is local and
  the vendor is the judge of the token. An account carries the single snapshot
  discovery took, and its label, ledger key and request credentials stay bound
  to it for the whole round, so a login that changes mid-refresh cannot put
  one account's response on another's row. The one later read — `check`
  sampling the token after a 401 — is diagnostic and replaces none of them.
- **Health is a table over that snapshot, first match wins**, with no vendor
  probe on the board path: file absent → no login; unreadable, or a ChatGPT
  login without a string access token → bad blob; another `auth_mode` (API
  key, agent identity, Bedrock) → unknown, captioned with the mode, no usage
  read; otherwise OK. "Relogin required" is never produced: the document
  carries no refresh-token expiry, and expiry is read only on positive
  evidence. A stale access token and an undecodable identity are *attempt*
  facts that block headroom's request and say nothing about the account.
- **The ledger key is the account and the person**,
  `uuid:<account_id>/<user_id>`, both required: two homes on one login share a
  budget, two people in one workspace do not. The spelling is permanent —
  changing it would orphan every Codex quiet period. An account that cannot
  say whose it is has no key at all: it makes no claim and replays nothing,
  and the dir-name fallback Claude Code accounts use is closed to it.
- **The usage endpoint** is `GET https://chatgpt.com/backend-api/wham/usage`
  with `Authorization: Bearer <access_token>` and `ChatGPT-Account-Id`. The
  body names whose it is (`account_id`, `user_id`, `plan_type`), and every
  response — live or replayed — is checked against the account: one naming
  another account or user is recorded as unparseable and never shown. Its
  budget is unmeasured; the spacing starts at Claude Code's value as a local
  policy, not a vendor promise. The plan shown is the stored response's when
  there is one and the id token's otherwise, chosen when facts are assembled
  from disk — so `limits`, which never reaches the access reader, reports the
  plan the board does. The same client exposes a POST that spends a
  rate-limit reset credit; headroom only ever issues the GET.
- **Windows keep the vendor's words.** A row's `kind` is the slot (`primary`,
  `secondary`), its `group` the limit object it sits in (`rate_limit`,
  `code_review_rate_limit`, `additional`), `feature` the `metered_feature` of
  an additional limit, `window_seconds` the stated duration — and all of them
  are the column identity, so two accounts whose primary slots run for
  different lengths get different columns. Severity is always `normal`: the
  vendor sends none, and a reached limit is the allowance's to carry. The
  secondary, code-review and additional shapes are built from the vendor's
  source and have not been seen live.
- **An unstarted window is a fact about the row, not a fourth field state.** A
  never-used account reports 0% with a reset one full window away that slides
  forward on every request until first use (measured once). The row reads "not
  started", its reset state stays `none` and its instant 0, so it can never
  read as rolled over. The predicate is deliberately narrow — every field
  decodes, nothing spent, remaining time at least the whole window — because a
  missed unstarted window shows a harmless full-window countdown while a false
  one would hide a real reset.
- **The allowance is the account-level half of the response.** Positive,
  well-typed evidence blocks — `rate_limit.allowed` false, `limit_reached`
  true, `spend_control.reached`, a `rate_limit_reached_type` naming a kind (on
  the wire an object, `{"type": …}`; an unrecognized kind still blocks and is
  shown verbatim) — and only that: a blocking field that does not decode, an
  empty kind included, is drift that never blocks,
  and unknown (every Claude Code response) is never read as allowed.
  `Actionable` consults it, so a blocked account is not offered as grounds for
  a choice whatever its percentages say, and the footer counts it apart from
  figures that are merely old. A block on one feature — code review, an
  additional limit — is a caption and leaves the account usable. Codex itself
  treats spend control and the workspace reached-types as hard stops, which is
  why they are account-wide here.
- **Ambient credentials outrank the home.** `CODEX_ACCESS_TOKEN` is read ahead
  of `auth.json` unconditionally (measured) and `CODEX_API_KEY` on the exec and
  main CLI paths (source); `CODEX_SQLITE_HOME` re-points the session index.
  The launch constructor strips all of them with `CODEX_HOME`, and a
  credential's value never reaches a notice or a `check` line.
- **One shared session store.** With only `sessions/` symlinked to
  `~/.codex/sessions`, a second home resumes the first's sessions, builds its
  own index by backfill and finds sessions created later; a session created
  under one account, encrypted reasoning included, continues under another.
  That is the topology `accounts add --vendor codex` seeds and launch
  verifies, and it is why continuing on another account is naming it:
  `headroom launch --vendor codex --account <other> -- resume`, or
  `-- resume --all` for every project.

### What Codex does not get, and why

- **No usage from session files.** Rollouts carry per-turn rate-limit records
  but no account identity, and under the shared store one rollout holds turns
  from several accounts; attributing one would show an account's quota under
  another's name. Before headroom's first fetch a Codex account's usage is
  unknown.
- **No `accounts remove`.** Claude Code's removal is gated on a live-session
  registry; Codex has none headroom can read, and an empty read is not proof
  of inactivity. The command refuses outright, with and without `--yes`, and
  names the directory to delete by hand.
- **No Codex rows in `headroom sessions`.** Resuming across accounts is the
  vendor's own picker over the shared store, so no session reader, ownership
  model or re-home exists for Codex.
- **Only the file credential store.** A keyring login reads as not logged in.
  `check` runs `codex login status` per home, through the launch environment
  constructor, and reports a login with no `auth.json` as an undetermined
  credential source; it also asserts that an empty `CODEX_HOME` answers "not
  logged in", the isolation everything above rests on.
- **A 401 is never drift.** Codex recovers from one by reloading and
  refreshing, so on the board it is an attempt caption and in `check` it is
  inconclusive, where Claude Code's unchanged-token 401 fails.
- **No `/login` hint.** Codex logs in with one command, so every hint names
  `headroom launch --vendor codex --account <name> -- login` in the engine's
  own spelling — a wrapper's argument passing is not headroom's to know.

## Dependencies and shape

stdlib plus `golang.org/x/term` (raw-mode terminal control for the two
pickers), nothing else: no CLI framework, no TUI framework — the interactive
surfaces share terminal lifetime and decoding, with their own frame policies.
No cgo either:
the Keychain is read by exec'ing `security(1)`, which keeps builds trivial.
Parsing is pure functions (`[]byte` in, tagged structs out) tested by table;
exec and HTTP stay in thin edges.

`internal/tui` owns the terminal session whole: raw-mode lifetime,
restoration on SIGTERM/SIGHUP/SIGINT (restore first, then re-raise so the
exit status stays honest about the signal), and incremental key decoding —
escape sequences may split across reads, and a bare ESC resolves as a
keypress only after a pause.

## Verification

`make check` runs vet and the race-enabled Go suite. Parser tables pin vendor
contracts and visible drift. The placement rule is a table over candidates;
the store's place operation is tested for the same property as the claim —
contending handles on the real lock — and the launch path through the exec
seam with credentials and the process table injected. The detached refresh
is the exception: it is exercised through the built binary against a local
endpoint that stalls, since the property is that the launch returns first
and a process that outlived it records the answer. The launch operation's
tests drive both of its callers, `launch` and the session picker.
Operation tests exercise request preparation,
local HTTP and the real store together, so sent credentials, received facts,
refusal backoff and persistence are tested through the callers' interface.
Checker tests use fixture processes and HTTP to verify the final exit verdict.
Store tests own locking, generations, migration preservation and cooldowns;
launch tests own routing, refusal-before-persistence and failure-after-write.

A second home is tested at each tier. The store's tests drive a handle on
each home's root over the real lock: a claim from either denies the other, a
launch's load lands in the shared ledger and its re-home in its own home,
each home's sweep leaves the other's records, a refused move leaves the
ledger untouched, each half-written outcome is reported as the half that
landed, and launches from both homes started together spread evenly. The
launch operation's tests build two homes logged into the same subscriptions:
each counts the other's busy sessions and recent launches, a home that left
stops counting at once, each subscription is asked once across both homes'
refresh rounds, no launch resolves the other home's dir whatever it
inherits, each picker lists its own store, and the owner's board and
`--json` name the other home's load. `check` audits each of a sharing home's
files on its own and judges only current members. Through the built binary,
a launch with `HOME` at the second home, no terminal on any stream and a
launchd-sized environment runs both of a session's turns on one of that
home's dirs, records its load in the owner's ledger and its routing in its
own file; a refresh killed mid-request leaves the shared store readable,
unlocked and askable after one spacing; and one sent SIGTERM — mid-request
or mid credential read — stops within a second, having claimed nothing it
could not complete.

`make test-pty` (`test/pty/`) covers what Go tests cannot observe: actual picker
interaction, selection, refresh scheduling, scrollback and terminal lifetime.
Automatic placement is exercised there through the real binary: five launches
started together reach five accounts, a session named on its first turn
resumes on the same account, the board under auto marks an account's row,
and `a` and enter move one vendor's `.current` between the word and a pin.
Both vendors' accounts roots and usage URLs are fixture values from the
harness's first command, because `HEADROOM_HOME` does not override an accounts
root a caller's shell exports. Codex's fixture root does not exist until its
block creates it — so every frame before that is the single-vendor one — and
Codex is present for one block that pins the two-vendor print,
tab switching, and enter on the Codex page writing Codex's `.current` alone.
Successful session launch must hand the child the restored terminal mode;
signals must restore it before exit. The harness uses fixture `claude` and
`security` commands and redirects account and transcript state into a temporary
home. Signal cases address the recorded fixture child PID. An outer shell
checks terminal state after exit; the tmux scrollback case skips when tmux is
absent. The dotfiles repo's separate sandbox harness tests shell wrappers
against this checkout's binary and a recording `claude` stub.

## Status

Personal-first: the defaults fit the author's machine, and `HEADROOM_*`
environment variables re-point each of them. The endpoints headroom reads are
undocumented and restricted by the vendors' consumer terms to first-party
clients; this repository exists as a personal, read-only instrument, not as
a distribution.
