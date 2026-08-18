# Design — why headroom is shaped this way

headroom is a read-only observer of a system it doesn't own: several Claude
Code logins on one machine, each keyed to its own config dir. It answers
"which account has headroom left?" and lets the user act on the answer (the
account board, `accounts`: live and self-refreshing on a terminal, one frame
off it, a versioned document under `--json`), answers "which session do I get
back into, and on which account?" (`sessions` — see The session surface below),
turns the chosen account into a running session (`launch`/`resolve` — see
The launch surface), and proves its own assumptions still hold (`check`).
This file records the mental model and the vendor contracts — the things the
code can't say about itself.

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
- **Two one-line files beside the accounts.** `.current` names the account
  bare `x` targets next — written atomically by headroom alone (the board's
  enter, `launch --remember`) and read strictly by headroom alone
  (`accounts.Select`; see The launch surface for why corrupt state refuses
  rather than defaulting). `.order` (optional; one email per line, `#`
  comments) sets display order after the primary; unlisted accounts follow
  alphabetically — configuration a human edits. Both stay outside
  `state.json` on purpose; see The one file headroom writes.

Read-only means read-only *against Claude Code*: headroom never writes the
Keychain, never refreshes a token, and never touches vendor login or quota
state — Claude Code owns all of it. What it writes is its own: `.current`,
and one `state.json` holding non-secret request timestamps, the usage
responses it fetched itself, and explicit session re-homes. Three documented
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
  from the primary's `~/.claude` (`accounts.SharedConfigEntries` —
  settings, skills, commands, agents, hooks, plugins, …), and
  `--share-config=<dir>` links every entry of that dir (a config package,
  which holds config and nothing else). Whitelist and not blacklist because
  a config dir also holds what must stay per account: `history.jsonl` is
  session-ownership evidence, `sessions/` the live registry,
  `.credentials.json` a login on machines without a Keychain. The list is
  vendor-perishable, but nothing load-bearing rides on it, which is why
  `check` does not verify it. Seeding never writes `.current` or `.order`.
  A dir that fails half-way is left in place and named: it is inert
  (discovery lists it, launch refuses it on topology) and deleting a
  directory to tidy up is not seeding's call.
- **`accounts remove <email | name.lock>`** runs its refusals before
  anything irreversible: not the primary (Claude Code's own default dir has
  no entry in the accounts root); the liveness gate (`sessions.Liveness`
  over the dir's own registry — verified live refuses, and so does
  "could not verify", the same rule as `dd`); then confirmation — the dir
  name typed back on a terminal, `--yes` off one, and refusal without
  either. Then the Keychain item, then the dir (`os.RemoveAll` does not
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
- **`.current` and `.order` stay out of it.** `.current` is one human-legible
  routing fact with its own failure policy — corrupt refuses the launch,
  visibly — while this document's ledger section is disposable and
  self-heals by quarantine; folding the two together would put a routing
  decision under the ledger's corruption policy or vice versa. `.order` is
  configuration a human edits, comments included.

Two policies fall out of the budget. Figures are labelled stale only once they
are older than the request spacing — inside that window no newer answer is
obtainable, so nagging about age would be nagging about something nobody can
act on. And the board's refresh cadence *is* eligibility: there is no interval
to configure, because asking sooner is refused and asking later says less
than it could. Presence bounds it: a board left open on a spare tab stops
asking after a few minutes without a keypress, since a surface that polls for
hours with nobody looking is the background daemon this design declines to
have, wearing a TUI.

`r` means *ask now*, and the claim — never the board's loop — answers. A
round has two halves: a local half that re-reads everything free (health,
discovery, credentials, `.current`, the store's replay of what another
surface may have bought seconds ago) and a budget half, the claim plus
whatever fetches it permits. `r` runs both immediately; an account still
inside its quiet period comes back annotated with when it can next be asked,
never silently re-armed into the cadence — pre-deciding eligibility in the
loop was the duplicated-eligibility bug the claim exists to close. It never
buys an exemption, because an override would spend against a bucket where a
refusal can itself cost more than it buys. The one veto is a 30-second floor
between rounds — spawn hygiene for the local half's per-account auth probe
under a held key, never budget arithmetic — inside which the press queues a
round that fires the moment the floor passes, unattended included, because
it was asked for. The board acknowledges a manual round in the status line
when it lands ("refreshed · usage in 40s"), briefly: a claim that persists
past its moment is how the old wording read as being ignored.

## The limits surface: reading without spending

Machine consumers exist — a statusline refresher wanting one account's
weekly numbers a few times a minute — and the board's `--json` is the wrong
instrument for them twice over: it probes health for every account
(`claude auth status` at ~170ms each is the dominant term of its ~300ms
warm), and it may spend every account's request budget to answer about one.
`headroom limits [--account <name>]` is the read that fits: the same
versioned document, assembled from disk alone — discovery, `.current`, and
the newest stored observation through the same replay every surface uses
(headroom's own store and Claude Code's cache, newest wins). No health
probe, no Keychain read, no claim, no request: it is not a second door onto
the endpoint because it never touches the door at all, and it answers in
about 10ms. Refreshing what it reads stays the fetching surfaces' job — the
board, `--json`, `check` — all behind the one claim.

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
symlinks to `~/.claude/projects` (the dotfiles repo owns that topology) — so
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
  exec. The contract is exactly two states: empty means "no launch was
  committed — do not cd" (cancel and every refusal), non-empty means "this
  dir was entered" (regardless of how claude then exited). Nothing
  routing-relevant is representable in it, and deleting the flag changes
  no routing — it is advisory by construction. A write failure is one
  stderr line, never a refusal. Deliberately without `--remember`, which
  the exec makes tempting: `.current` means "where new sessions go", and
  only the account board moves it. This surface does no network I/O and
  never spends the usage budget.
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
  Every inherited `CLAUDE_CONFIG_DIR` is stripped; exactly one is set for a
  non-primary account; the primary is selected by the variable being
  *absent* — the only behavior verified against the binary (present-but-
  empty is unverified territory and is never produced). The decision crosses
  the seam only as a validated target ("primary plus a dir" and "extra
  without one" are unrepresentable), and auth's per-account probes build
  their environments through the same constructor, so a polluted shell can
  neither re-route a launch nor make every health answer come from one
  login.
- **`headroom launch [--remember] [--account <name>] [-- args]`** resolves
  the account (omitted `--account` means the recorded choice), optionally
  records it as where bare `x` goes next — before the exec, and a failed
  record refuses the launch — then execs `claude`, preserving stdio,
  signals and exit status by replacement rather than proxying. `--remember`
  is an explicit ask, never a launcher default: the wrappers' generated
  `x-<name>` launchers deliberately omit it, because a named launch means
  "this session, that account" — scoped — and a pin riding along as its
  side effect let a two-minute hop to another account silently retarget
  every later bare `x`. In ordinary use only the board's enter moves
  `.current`; the flag remains the scriptable spelling of that decision.
  Two more
  refusals sit before the record, so a refused launch can never move where
  bare `x` goes: a primary launch under a re-pointed `HEADROOM_HOME`
  (headroom would be describing one tree while the vendor, selecting the
  primary by the variable's *absence*, resolved another), and a broken
  shared-sessions topology (below).
- **The shared-sessions topology is headroom's invariant, verified at
  launch.** The session surface's whole model — one picker over one
  machine-global store — holds only while every extra account's `projects/`
  is a symlink resolving (by inode, not path spelling) to the canonical
  store; a violation forks session history silently. So `launch` verifies it
  before every extra-account exec and refuses with the exact end state
  required, `check` asserts it per account through the same verifier
  (`accounts.VerifyTopology` — one function, so the gate and the report
  cannot disagree), and *creation* deliberately stays with the wrapper's
  explicit seeding command: a launch that quietly repaired topology would
  hide exactly the state the refusal exists to surface.
  **`headroom resolve [<name>]`** prints
  `canonical-name<TAB>config-dir<TAB>kind` (kind: `primary`|`extra`) for
  shell preflight: the dir so personal checks can run against it, the kind
  so *which* preflight applies is headroom's classification — a wrapper
  that re-derives it by prefix-matching its own idea of the accounts root
  silently skips its checks whenever a `HEADROOM_*` override moves the real
  one. Resolve is advice, not a capability: launch revalidates whatever
  comes back.
- **Selection fails closed.** An absent `.current` is the documented
  fresh-start default (the primary); an empty, unreadable one, or one
  naming a deleted account, refuses with an actionable message — on the
  launch, in `check` (an own-state FAIL), and as an unset `← x` marker on
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
  that fires on the ordinary case is noise, so the board and the launch
  line stay quiet for it (`accounts.KnownExtraDir`, one classifier for
  both, beside the target's own conflict classifier so diagnostics and the
  environment built cannot disagree). What stays loud, because tools
  *outside* the managed path still obey the variable: a relative value
  (the stale-wrapper incident signature — an own-state FAIL in `check`), a
  value naming nothing discovered, and the primary dir spelled explicitly
  (present-but-primary is not the verified absent state). `check` reports
  every present value either way — full detail is what a diagnostic
  command is for.

headroom *advertises* only guaranteed launcher identities:
`x-<email>` per account dir, the configured name for the primary
(`accounts.Launcher`). Short local-part aliases are shell convenience,
invisible to headroom — the old shared naming policy ("keep the two rule
copies in sync") was a cross-repo contract that could drift. Wrappers own
what is personal: seeding, flags, aliases — and they never touch
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
included; the fetch pipeline's single-writer property, the claim's
test-and-set, launch environment construction (a polluted environment must
be stripped, never inherited) and the fail-closed selection states all have
dedicated regression tests under the race detector. The zsh→exec seam —
wrappers delegating to a real headroom binary — is covered by the dotfiles
repo's sandbox harness, which builds this checkout and pins routing
end-to-end against a recording claude stub.
What `go test` can't reach — signal-time terminal restoration, real picker
interaction — lives in the committed expect(1) harness (`make test-pty`,
`test/pty/`): SIGTERM must leave the terminal sane; arrows + enter must select
and write state; ESC must cancel writing nothing; the board must redraw its
own countdown from its own ticker and stay responsive under a held-down
refresh key (that the key cannot *become* traffic is the claim's property, and
is pinned in `go test`); the session picker must refuse a primary commit
under the harness's re-pointed home *into* a still-live picker with the
advisory cd file left empty (the exec success path is the dotfiles sandbox
harness's, which stubs a recording claude), leave the cd file empty on
cancel, survive SIGTERM inside the alternate screen, keep scrollback empty
while the board redraws in a pane shorter than itself (the one case that
needs a real scrollback buffer, so it runs under tmux and skips where tmux
is absent — expect alone remains the harness's requirement), and delete only inside
the harness's fixture store — `HEADROOM_HOME` re-points the primary config
dir and session store, and exists so a `dd` test can never touch the real
machine's transcripts (and is also why enter must refuse there: relocated
homes and primary launches don't mix). Its two hard-won patterns are documented in
`test/pty/run.sh` — kill with `pkill -nx headroom`, never `-f`, and inspect
post-mortem terminal state via an sh wrapper running `stty` in the same
pty.

## Status

Personal-first: the defaults fit the author's machine, and `HEADROOM_*`
environment variables re-point each of them. The endpoint headroom reads is
undocumented and restricted by the vendor's consumer terms to first-party
clients; this repository exists as a personal, read-only instrument, not as
a distribution.
