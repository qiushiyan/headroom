# Design — why headroom is shaped this way

headroom is a read-only observer of systems it doesn't own: several Claude
Code logins on one machine, each keyed to its own config dir, and several
Codex CLI logins, each keyed to its own home. It answers "which account has
headroom left?" and lets the user act on the answer (the account board,
`accounts`: live and self-refreshing on a terminal with a page per vendor,
one frame off it, one row per account under `--compact`, a versioned document
under `--json`), turns the chosen account into a running session — a new one,
or one continued on the account that last drove it (`launch` — see The launch
surface and Sessions), renews the logins that end soon in one pass, or one
account's across commands that each return for an approver elsewhere
(`login` — see Renewing logins), hands a program that runs its own sessions a
token and a placement (`headless` — see Headless tokens), and proves its own
assumptions still hold (`check`). This file records the mental model and the
vendor contracts — the things the code can't say about itself.

Most of this file is written about Claude Code, the vendor the model was
built on. The mechanisms it describes — discovery, the claim ledger, the
three axes, launch preparation — are the same code for Codex;
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
  constructs absolute dirs: `config.Load` refuses a relative `HEADROOM_HOME`
  (and never rewrites an accepted spelling — the Keychain hash is keyed on
  it), and `launch.Extra` refuses a relative dir at construction.
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
- **Small files beside the accounts.** `.order` (optional; one email per
  line, `#` comments) sets display order after the primary; unlisted
  accounts follow alphabetically — configuration a human edits. `.reserved`
  (optional, one `<account><TAB><dir>` per line, the account named as every
  record names it — see The one file headroom writes) keeps accounts for
  repositories, written by `accounts reserve` and read strictly: damage
  refuses every automatic choice rather than releasing an account — see
  Automatic placement. Both stay outside `state.json` on purpose; see The one
  file headroom writes. Nothing records where a bare launch goes: the rule
  decides it at every launch.

Read-only means read-only *against Claude Code*: headroom never writes the
Keychain, never refreshes a token, and never writes vendor login, quota or
session state — Claude Code owns all of it. `login` is no exception: it runs
Claude Code's own login command, and Claude Code stores what results. What it
writes is its own: one `state.json` per accounts root holding non-secret
request timestamps, the usage responses it fetched itself, session re-homes,
the record of recent launches and the headless runs; `launches.jsonl`, the
launch log nothing reads (see The launch log); `headless.json`, the one file
that holds secrets — the headless tokens, mode 0600 (see Headless tokens);
the `.login` directory, readable by the user alone, where each account dir's
login lock, handed-off sign-in and running vendor login are kept (see
Renewing logins); and, only when asked to, `.reserved`. One documented
exception, an explicit user command naming its object and refused while a
session is live or liveness is unverifiable: `accounts remove` deletes the
removed account's Keychain item (`creds.DeleteKeychainItem`, the one
Keychain write in the codebase, reached from no observation path). That is
deliberate: removing an account is not a read, the user's expectation of
"remove" includes the login, and a usable token left behind for a config dir
that no longer exists is the worse outcome. Nothing in it touches any *other*
account's item — the service name is derived from the removed dir's spelling
alone.

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
  caller choice, with one exception: a Claude Code dir gets the primary's
  `settings.json` whenever the share does not provide one, because a dir
  with none writes its own at its first session and prunes every account's
  history by the vendor's default. A primary with no such file has nothing to
  link, and the add goes on, since refusing it would stop a fresh machine:
  every account then reads one default, and `check` says so. Bare
  `--share-config` links a *whitelist*
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
  `check` does not verify it — except `settings.json`, whose
  `cleanupPeriodDays` prunes the shared store: `check` requires every
  account sharing the store to read one (§ Sessions: what a launch reads, on
  retention). Whatever the share mode, the entries that are never
  configuration (`accounts.PerDirEntries`: for Claude Code
  `.credentials.json`, `.claude.json`, `history.jsonl`, `sessions/` and the
  store link) are skipped and named: a config package that happens to hold
  a login must not make two dirs one login, and `check` fails on a dir
  whose login, history or registry is a link. Seeding never writes
  `.order`.
  A dir that fails half-way is left in place and named: it is inert
  (discovery lists it, launch refuses it on topology) and deleting a
  directory to tidy up is not seeding's call.
- **`accounts remove <email | name.lock>`** names its object up front: bare,
  it names the removable candidates (every extra plus any `.lock` debris; the
  primary is never one) and refuses, on a terminal too. Then it runs its
  refusals before anything irreversible: not the primary (Claude Code's own
  default dir has no entry in the accounts root); `accounts.CheckRemovable` —
  form, and the one refusal that guards data: a `projects/` that is a *real
  directory* holds sessions never migrated into the store, and `RemoveAll`
  would take them with the account; the liveness gate consumes
  `sessions.ReadRegistry` problems and `sessions.InspectClaims` evidence — an
  unreadable claim or registry directory refuses removal, as does any live or
  unverifiable session; then confirmation — `y/N` on a terminal, `--yes` off
  one, and refusal without either — and the gate once more after the reply,
  since the prompt may have stayed open while a session started. Then the
  Keychain item — deleted once `security` says it exists, and an item it
  cannot answer for stops the removal with the dir in place, since a token
  left behind is what removal exists to prevent — then the dir (including
  any file-backed `.credentials.json`; `os.RemoveAll` does not follow
  symlinks, so the `projects/` link goes and the store behind it stays —
  pinned by test), then every `.order` line discovery reads as that account,
  a trailing comment included, its headless token and its reservation — each
  cleared on its own once the dir is gone, since a re-run finds no account
  to remove: one that fails leaves the others done, and the error names what
  is left with what clears it.
  Transcripts survive (they are the machine's); a session the removed
  account drove is placed like a new one at its next resume.

`claude auth status` is used for its answer only. It may or may not refresh a
token as a side effect; headroom neither relies on that nor invokes it hoping
for it. Building on an unpromised side effect would be the same class of
mistake as reading `expiresAt` as account health.

### Renewing logins

A Claude Code login ends at its `refreshTokenExpiresAt`, which the vendor
fixes at login — about 27 to 30 days out, measured on 2.1.291 — and which
using the account does not move. Each config dir on each machine is its own
login, so an owner with several subscriptions on more than one machine meets a lapse
every few days unless the logins are renewed together, after which they end
together. `headroom login` is that batch; the owner's part is one Authorize
click per account.

- **The vendor does the login.** For each chosen account, in turn, headroom
  runs `claude auth login --email <email>` under the environment
  `launch.Prepare` builds for it, so the login lands in that dir and its
  Keychain item exactly as a session's `/login` would. `--email` pre-fills
  the sign-in page. Its output is the terminal's; when the browser cannot
  reach the vendor's localhost callback, the vendor asks there for the code,
  which headroom reads (below). A session running on the account during its
  renewal keeps working.
- **headroom is the vendor's `$BROWSER`.** Claude Code opens the approval page
  by exec'ing `$BROWSER` with the URL as its only argument and its own
  environment; a value carrying arguments is not run at all (both measured on
  2.1.291). So `$BROWSER` is the headroom binary, with
  `HEADROOM_BROWSER_PROFILE` naming a Chrome profile directory (empty for the
  default browser) and `HEADROOM_BROWSER_SIGNIN` a file for the URL's
  `state`. Called that way with one https URL, headroom records the state,
  runs `open(1)` and does nothing else — dispatched before configuration,
  like `version`, since the vendor's environment is not one headroom must
  accept.
- **headroom reads the pasted code.** Claude Code reads its code prompt with
  echo and line editing off, and sends whatever `code#state` it is given to
  the token endpoint without matching the state to its own sign-in (2.1.309):
  a paste never shows, a correction goes in as raw bytes, and a code from
  another page — one left open by an earlier attempt — comes back as a bare
  `Request failed with status code 400` that ends the attempt. So the
  vendor's input is a pipe from headroom, which reads the terminal in its
  normal mode, where a paste echoes and edits as a line, and passes a line
  on only when it is shaped `code#state` and carries the recorded state; the
  vendor builds the URL it opens and the URL it prints from one state. A
  refused line gets the reason and a fresh prompt, and the sign-in stays
  open. The same line again goes through as pasted: the check is headroom's
  reading of the vendor, and must never stand between the owner and a
  login. With no state recorded — off macOS at the machine, or no opener
  call within a few seconds of the paste — a well-formed code goes through
  without the state check, and headroom claims none. The pipe costs the vendor's
  paste-only fallback for a localhost listener that cannot start, which it
  offers only on a terminal.
- **The profile is matched, never configured** (`internal/browser`, reading
  Chrome's `Local State`): the profile signed in to the account's email as a
  Google account; failing that, the one named by the email or its local part,
  among profiles not signed in to some other Google account. Exactly one
  must match, or the page opens in the default browser and the plan says so.
  The sign-in exclusion is load-bearing on the author's machines: `Default`
  is named after the owner and signed in to the primary's Gmail, while an
  extra whose local part is the same name has its own profile. A profile
  list that cannot be read is named as such, never as a failed match: a
  sandboxed agent's shell is refused `Local State` outright ("operation not
  permitted").
- **Which accounts.** Names choose outright, `--all` chooses every account;
  otherwise only positive evidence does, read as the board reads health: no
  login, an end already passed, or an end within `--within` days (default
  7). An absent or unreadable expiry is never read as near.
- **A Keychain that withholds a login holds its account back.** macOS
  refuses an ssh session the login Keychain ("User interaction is not
  allowed") until `security unlock-keychain` runs in it, and a locked
  Keychain refuses any session; Claude Code then reads and writes the file
  store, and `claude auth status` reports an account whose login lives in
  the Keychain as logged out. The item's attributes stay visible, so the
  account is judged on what the Keychain gave: an item that exists while
  the login did not come back from it marks the account unreadable here,
  never renewed from here, even by name — its state is unknown, and a login
  made here would land in a file that any session able to read the
  Keychain ignores, since a readable item wins. An item `security` cannot
  answer for — any exit but found or not found (44), or no answer in ten
  seconds — holds the account too: unknown is never read as absent. The
  sealed probe only words the explanation. Accounts with no item are file-backed
  everywhere and renew normally. `creds.Reading.Access` is the one rule —
  served, withheld, or unplaced — and the plan, the handoff's code check,
  the read-back, the board's health and `check` all judge a read by it.
- **A remote user gets the URL, not a window.** When `SSH_CONNECTION` names
  another machine as the client, the page would open on a screen nobody is
  at, so headroom as `$BROWSER` records the sign-in and opens nothing: the
  vendor prints its URL, the user opens it where they are, in a browser
  signed in as the account, and pastes the code it shows. An ssh from a
  machine to itself — how the author's mini attaches its own tmux — keeps the
  profile opener. Off macOS, at the machine, the vendor keeps its own browser
  handling.
- **The result is read back**, against the credential as it stood just
  before that account's login. A login took only when the vendor command
  succeeded, the refresh expiry is a new one, read where sessions read it,
  and the dir's `.claude.json` now names the account's email — an extra's
  dir name, the primary's previous login or, failing that, the vendor's
  status. A renewal readable only from the file while the account's
  Keychain item exists did not take: no session that reads the Keychain
  uses it. A vendor command that failed is a failure whatever changed
  meanwhile, since the change may be another login's. A changed access
  token is not evidence: any live session on the account refreshes it while
  a batch waits on other approvals. Approving in the wrong profile is the
  failure this catches at once, instead of later as the board's `(dir says
  …!)` — but only after Claude Code has stored that login over the dir's
  previous one. A primary with a login and no readable identity is held
  back like a sealed account, since its new login could not be checked.

The vendor can end a login before its recorded expiry — on the author's mini,
logins made on different days were refused within the same half hour — and
nothing in the stored credential shows it. Health then reads fine until a
request fails; naming the account renews it.

### Renewing from elsewhere

An approver that is not at this machine's terminal — an agent with shell
access, which signs in to claude.ai as the account in a browser of its own,
or the owner on another machine — cannot hold the batch's one conversation
open: it works in calls that each return. `headroom login start <account>`
and `headroom login finish <account>` split one account's renewal into two
such calls, `status` and `cancel` inspect and end it, and `headroom login
--dry-run --json` is the plan as a document. Each prints one JSON document,
a state and a stable reason; exit 3 means refused with nothing spent or
sent. The commands and the document an agent reads are in
`docs/specs/login-handoff.md`, the dated record of the decision and the
contract. What the shape rests on, measured on 2.1.311:

- **The vendor must outlive the call that starts it.** It holds the PKCE
  verifier, and headroom must not exchange the code itself. So `start` hands
  the account's `claude auth login` to a holder (`login hold`, hidden) in a
  session of its own, with no stream of the caller's — `ssh host headroom
  login start x` returns — and the holder is the vendor's parent until the
  sign-in ends. The vendor's input reaching end of file does not end it: a
  killed holder leaves it waiting forever.
- **The URL handed out is the one the vendor printed.** On a pipe it prints
  plain lines, and its printed URL redirects to the page that shows the
  code; the one it hands `$BROWSER` redirects to a localhost listener a
  remote browser cannot reach. Both carry one state. `browser.SigninURL` is
  the one reader of that screen; headroom, as a `$BROWSER` that records and
  opens nothing, cross-checks the state, and never builds a URL itself.
- **A code is sent once, and only one this sign-in's state carries.** A
  well-formed code the server rejects ends the sign-in; a malformed one
  leaves it waiting. So the holder refuses a code that is not `code#state`
  or carries another state, with no repeat-to-bypass — the URL was
  headroom's own, so another state is another sign-in — and checks the
  Keychain still gives the login before sending. `signin.Signin` owns the
  sign-in's steps: `exchanging` on disk is the authorization to send, so a
  code that cannot be recorded is not sent and a dead holder's record never
  says no code was sent when one was, while an ending is what happened
  whether or not it could be saved.
  The ten-minute wait for a code stops once one is sent; the exchange has
  its own bound, after which a vendor headroom had to end is judged by its
  read-back alone.
- **One login per account dir, whoever runs it.** Under the accounts root's
  `.login`, keyed by the dir, sits a lock held by whatever runs the
  vendor's login — the holder for its whole life (kept from the vendor, and
  referenced so the collector never closes it), the batch while its login
  runs — so two logins never race on one credential, and a second `start`
  returns the live sign-in. The vendor is registered beside the lock from
  its launch, by the holder before any URL and by the batch alike, and runs
  only while registered. Whoever next holds the lock ends a registered
  vendor its dead owner left — by pid and `ps` start, after the exchange
  bound if a code may be in flight — and settles the record, undated:
  nobody saw the holder go, so no surface gives that sign-in a finish time.
  A holder that takes a call and does not answer in time is slow, not gone.
- **An unattended pass is read back from the plan.** An ending names the
  holder's log, where the vendor's own words are kept until the account's
  next `start`, and the plan's line and document name each account's last
  sign-in that did not renew — one its holder left included, dated by its
  start — since the one command that shows every account is where the
  owner learns what an agent did.
- **A unix socket is the rendezvous**: dialing it says whether a holder is
  alive, and a request carries a protocol version a holder from another
  build refuses before acting. A socket binds at 103 bytes on macOS, so a
  longer accounts root refuses `start` plainly.

### Headless tokens

A caller that runs sessions under a config dir of its own cannot be handed an
account dir, and must not be lent its login: a login refreshed from a dir
that does not hold its lock is a login two processes refresh. `headroom
headless` gives it a token per account instead, and a placement; the caller
starts the session itself. Two callers do: the skill-eval runner, and
planlab's steward, a pipeline that runs under a `HOME` of its own and
reaches this home's headroom with `HEADROOM_HOME` (its launcher prefix
places, takes the token, reports its own pid and execs `claude`). A second
set of logins for such a caller would be a second monthly renewal cycle for
every subscription it spends; a token lasts a year. The commands and the
document a caller reads are in `docs/specs/headless.md`, the dated record of
the decision. What it rests on, measured on 2.1.296:

- **`claude setup-token` is `auth login`'s flow with a narrower grant**:
  scope `user:inference`, a lifetime of 365 days, no `--email`. It execs
  `$BROWSER` with the URL as its only argument, so `mint` opens its page as
  `login` does; the code goes to the vendor's own paste prompt, since its
  input must stay the terminal (below). It stores nothing — it prints the
  token — so `mint` runs it under a scratch config dir and the account's
  dir, its login included, is never touched.
- **Its screen is read off a pipe.** With its output on a pipe it draws
  whole frames of plain lines, unwrapped; its input must stay the terminal.
  `headless.Scanner` is the one reader of that screen: it keeps the token and
  the lifetime the success line states (`valid for 1 year`, from the granted
  expiry), and withholds every `sk-ant-` string from the terminal. While it
  waits for the approval the vendor ignores every key, ctrl-c included, and
  exits on SIGTERM or SIGHUP, so `mint` bounds the wait.
- **A token says nothing about whose it is.** `claude auth status` with only
  `CLAUDE_CODE_OAUTH_TOKEN` set answers `authMethod: "oauth_token"`, logged
  in, with no identity — a made-up token reads the same. The account is the
  profile it was approved in; `mint` records the dir's identity at that
  moment, and a placement excludes the account once its dir names another.
- **A token outranks a stored login.** A config dir holding a login, an
  expired one included, answers `authMethod: "oauth_token"` once
  `CLAUDE_CODE_OAUTH_TOKEN` is set (measured on 2.1.319), so a caller's own
  config dir can keep whatever login it once had.

A placement is the automatic launch's rule over the accounts that hold a
token and that the caller has not excluded, both decided inside the locked
step that records the choice, so nothing is placed on an account the caller
would veto. One intent the launch never sets, `CanWait`, refuses when every
account left is near a limit and says when the first has room again, instead
of taking the least spent: an eval can wait; the owner's sessions on that
account cannot. The caller reports the session's process — a launcher that
execs the vendor reports its own pid, which the exec keeps along with its
start — and that *run*, kept in `state.json`, is a busy session of its
subscription on every surface for as long as its pid runs with its recorded
start: the registry claim's verification, on a record the caller supplies.

A token leaves `headless.json` only through `headless token <account>`: no
listing, document, log or `state.json` record carries one. A token file that
others can read is refused rather than repaired, so the owner sees that a
secret may have been read, and one that does not decode is never written
over.

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
  relative `HEADROOM_HOME` — is an own-state FAIL, never INCONCLUSIVE:
  every launch is refusing on it. So is a home re-pointed by
  `HEADROOM_HOME`, from which a primary launch refuses: `check` is run as
  the home it describes, never from a process that only reaches it, such as
  a headless caller. A claim failure
  marks the API untested; a completion failure leaves received vendor
  evidence available to check.
  FAIL takes precedence when a run also has inconclusive assertions. Every
  request uses the board's budget and storage operation. Its per-account
  `credential[...]` assertion names the selected source and requires a
  parseable blob; no credential is FAIL (not logged in), and a login the
  Keychain holds and did not give, or that cannot be placed, is untested.

Two deliberate tolerances beyond strict inherited behavior: numeric-epoch
`resets_at` values are accepted, and timestamps may carry offsets or
fractional seconds. Handled drift beats flagged drift.

## Three axes, because three things vary independently

`internal/accountstate` owns the facts shared by the command surfaces.
Health, observed usage and the latest request vary independently: a usable
account can have old observations and a refused refresh at the same time.

- **Health** — can Claude Code use this account? Only a new login (`/login`
  in a session, or `headroom login`) fixes a bad answer. Sourced from `claude auth status`, with credential evidence as
  fallback. A bad answer counts only when the login sessions use was within
  reach: when the read did not come from the Keychain and the account's item
  exists, or `security` cannot say, health is unknown and the caption says
  where the login is kept, never `/login` — a login made from that session
  is one its other sessions ignore (see Renewing logins).
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
  live board and the one-shot print can neither compute a column nor disagree
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
ledger, the responses, the session re-homes, the recent launches and the
headless runs.

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
- **One store per accounts root, under one lock.** Each vendor's
  `state.json` sits under its own accounts root beside its own `.order` and
  `.reserved`, so nothing in Claude Code's files is re-keyed by Codex existing,
  and the store a body was read from says which vendor's parser reads it. Both
  roots derive from `HEADROOM_HOME` (`$HOME` unless set) as `.claude-accounts`
  and `.codex-accounts`, so they can never name one location. Every write
  takes the file's one lock, so a launch's load and its session's re-home land
  in one write. A headless caller that runs under another `HOME` reaches this
  file through `HEADROOM_HOME` and the same lock: its placements and runs are
  this home's load.
- **A record names an account by its dir, never by what it is called.**
  What a person or a launch writes about an account — `.reserved`, the
  re-homes, headless tokens — names an extra by its dir and the
  primary `.primary` (`accounts.PrimaryRef`), which discovery never gives a
  dir. The primary's name is the one that can change: a pin
  (`HEADROOM_PRIMARY_NAME`, `HEADROOM_CODEX_PRIMARY_NAME`) or a new login
  renames it. So a name is what a person types and reads, translated at the
  edges (`Set.RefOf`, `Set.NameOf`), and shells holding different pins read
  one set of records; a record that kept the name would strand on every
  rename and chase it on every run. The launch log keeps names: it is
  history.
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
  rewritten. A file that is not JSON gives a reader no section — its
  re-homes are unreadable, never none — and the write that replaces it
  first keeps it beside itself under a name nothing holds
  (`state.json.unreadable`, numbered on), or does not replace it.
- **Records are bounded by age, never by which accounts a caller could see.**
  Sweeping by absence needs a complete account registry, and no caller can
  promise one: an account's key comes from a vendor file Claude Code rewrites
  constantly, so a torn read moves the key and the sweep deletes the live
  cooldown and stored answer of an account sitting right there.
- **Launches are recorded beside the ledger, and are as disposable.** The
  `placements` section holds the launches of the last fifteen minutes — the
  load a new launch must count — and the newest launch per subscription,
  which breaks ties. Load is keyed like the ledger, by the account's
  identity, because two dirs on one account spend one quota. A section that
  will not decode is set aside and the record starts empty: that costs a few
  launches their view of each other and nothing else. A binary from before
  the section existed carries it through its writes untouched.
- **`.reserved` and `.order` stay out of it.** `.reserved` is a person's
  statement with its own failure policy — damage refuses every automatic
  choice, visibly — while this document's ledger section is disposable and
  self-heals by quarantine; one file would put a reservation under the
  ledger's corruption policy or the ledger under the reservation's. `.order`
  is configuration a human edits, comments included.

Sections nothing reads — `legacy_import`, the checkpoint of a one-time
upgrade from `.owners` and `.throttle`, and `members`, a registry of homes
that once shared one file — are carried through writes untouched, like any
section this binary does not decode.

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

The interactive board is one page per present vendor, and it is read-only:
no key on it records routing or starts a session. It once pinned the row
under its cursor on enter, and a stray enter on a board opened only to read
it took every later bare launch out of automatic placement; choosing an
account is naming it for one launch. A page owns its list, scroll position,
schedule and its in-flight round, so a result lands on the page that started
the round and never on whichever page is visible when it arrives. Only the
visible page starts rounds: a hidden page keeps its figures and their age,
its deadline never wakes the loop, and one that passed while hidden is
simply due at the next tick. Tab switches pages, the arrows scroll a board
taller than the terminal, the tab bar ranks with the header when the
terminal is short, and quitting exits 0. With one vendor there is no tab bar
and no heading. Off a terminal each present
vendor runs one round, side by side, and the pages print in vendor order
under a heading each.

`r` means *ask now*, and the claim — never the board's loop — answers. A
round owns its account list and result channel until the channel has closed
and drained. Its local half re-reads everything free (health,
discovery, credentials, the store's replay of what another
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
the same versioned document, assembled from disk alone — discovery and
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
another group), or that shares its identity with another row of the same
response — tagged at the parse, both rows, so no reader chooses between
them and placement counts neither as room — is tagged
`identity_state: "bad"` and counts as drift, so
`check` fails before a consumer quietly stops matching — the scoped-weekly
case matters most, because the model-scoped weekly is routinely the binding
limit while the all-models figure sits far lower, and a scoped row
mislabeled as all-models would read calm at the exact moment work stops.

## Sessions: what a launch reads

Session transcripts are machine-global — every account's `projects/`
symlinks to `~/.claude/projects` (`accounts add` creates that topology) — so
any account can continue any conversation: `claude --resume <id>` from the
session's project, on whichever account the launch decides. headroom reads
the transcripts for nothing but their ids. What a launch needs about a
session is who drove it, whether it is running, and whether its record may
be swept; its vendor contracts, reverse-engineered and perishable:

- **Owner = the newest account claim headroom can observe or was explicitly
  given** — transcripts carry no account identity, so "the account that
  drove this session" is evidence, not ground truth, and the contract says
  so (`sessions.OwnerOf`). Three sources, one precedence: a *verified*
  live-registry claim (that account runs it now) beats everything;
  otherwise the newest of the explicit re-home and each account's newest
  `history.jsonl` prompt for that session id — all timestamps from this
  machine's clock, on one axis, never re-stamped. Attribution parses the
  decoded `sessionId` field, never substrings: prompt bodies quote other
  sessions' UUIDs. No evidence, a newest re-home to an account that is gone,
  or two accounts claiming the same instant each name no owner, and the
  launch places the session as it would a new one — never on the primary,
  which no evidence chose.
- **A re-home is the one fact the vendor never records.** An automatic
  launch that places a session it was given by id somewhere other than its
  owner writes one: a print-mode session leaves no prompt history at all, so
  without it each later turn would be placed afresh. Any newer evidence
  supersedes it. The sweep that retires re-homes of deleted transcripts runs
  inside the same lock, from a listing of the store taken there, concludes
  nothing from a listing that failed anywhere, and leaves a record younger
  than ten minutes alone — a first turn's id is known before the vendor has
  written a line.
- **Retention is one setting, read per account.** Claude Code's cleanup
  sweep deletes transcripts older than the `cleanupPeriodDays` in its config
  dir's `settings.json` (30 when unset), and through the store link every
  account's sweep prunes the one shared store — so the store keeps only the
  shortest period any sharing account names. `check` reads each sharing
  account's value and fails when they disagree; an unreadable or rejected
  value (below 1) is untested, since Claude Code pauses its sweep then too.
  Once a second account shares the store, agreement today is not enough:
  `check` also fails when sharing accounts read separate `settings.json`
  files (compared by inode, the primary included — separate files agree
  only until one is edited) and when any leaves `cleanupPeriodDays` unset
  (the default is the vendor's to change, and a lower one prunes every
  account's history at once). A primary alone shares nothing and is only
  reported. The value itself is the user's policy, printed on the ok line,
  never judged. Proving cross-account resume end to end would take a real
  request on two accounts and leave a transcript in the store, so `check`
  does not: the topology line is the static proof, and every resume on
  another account exercises the rest.
- **Liveness is pid + start instant.** Each account dir's
  `sessions/<pid>.json` registers a running session. A claim counts only
  when the pid is alive *and* its kernel start time matches the registry's
  `startedAt` within tolerance — pids recycle. (Epoch ms, not the
  `procStart` string: that one is UTC-rendered while `ps` speaks local
  time, so string equality fails everywhere but UTC.) The same record
  carries `status` — `busy` or `idle` on 2.1.286, `shell` seen once — which
  the reader passes through verbatim; only `busy` is given a meaning, by
  placement, and no other word is thereby "idle". Each pid is sampled once
  per reading, so ownership, load and liveness agree. Proven absence or a
  recycled pid clears a claim; failed inspection stays unknown. Removal is
  the mutation liveness gates: a live or unverifiable session refuses it, a
  damaged record refuses it whether or not it names a session, and so does
  a registry directory that cannot be listed; `check` names each.

A verb retired from the command line is an unknown word: clikit refuses it
before any configuration is read, with exit 2 and nothing on stdout, so a
stale shell function that still calls `sessions`, `resume`, `resolve` or
`launches` stops instead of reading a reply it would misparse. Those names
are never rebound to a new meaning (The launch surface says why).

## The launch surface

Launch routing is headroom's own semantics, not the shell's. The incident
that forced this: a tmux server started inside a Claude Code session carries
that session's `CLAUDE_CONFIG_DIR` in its global environment, and a wrapper
that set the variable for extra accounts but left the ambient environment
alone for the primary silently routed every "primary" launch to whatever
account that session ran on — while the board truthfully reported the
primary as the target. The invariant, and the reason `internal/launch` exists:

- **The child environment is a total function of the validated decision.**
  The variables are one policy table with a row per vendor
  (`config.EnvPolicy`), applied by one constructor; a target strips only its
  own vendor's variables. For Claude Code, every inherited
  `CLAUDE_CONFIG_DIR` is stripped; exactly one is set for a
  non-primary account; the primary is selected by the variable being
  *absent* — the only behavior verified against the binary (present-but-
  empty is unverified territory and is never produced). The decision crosses
  the seam only as a validated target ("primary plus a dir" and "extra
  without one" are unrepresentable), and auth's per-account probes build
  their environments through the same constructor, so a polluted shell can
  neither re-route a launch nor make every health answer come from one
  login.
- **`headroom launch [--account <name>] [--dry-run] [-- args]`** decides
  the account — bare, the rule's (see Automatic placement); a name, that
  account for this launch alone — then execs
  `claude`, preserving stdio, signals and exit status by replacement rather
  than proxying. Nothing records a choice for later launches. A recorded
  default was tried: whether a board's enter or a flag set it, a pin made by
  mistake silently took every later bare `x` out of placement, and naming an
  account per launch covers every deliberate use. A launch calls
  `launch.Prepare` before anything is written: routing,
  relocated-primary refusal (a primary selected by absence under a
  re-pointed `HEADROOM_HOME`), shared topology, executable path and child
  environment are resolved together. If process replacement fails after
  persistence, a re-home remains recorded and the error says so, from what
  `state.Place` reports it wrote; a launch that owed its session a re-home
  and wrote none says that before the vendor starts.
- **The shared-sessions topology is headroom's invariant, verified at
  launch.** Continuing a session on another account — the whole point of
  one machine-global store — holds only while every extra account's `projects/`
  is a symlink resolving (by inode, not path spelling) to the canonical
  store; a violation forks session history silently. So `launch` verifies it
  before every extra-account exec and refuses with the exact end state
  required, `check` asserts it per account through the same verifier
  (`accounts.VerifyTopology` — one function, so the gate and the report
  cannot disagree), and creation belongs to `accounts add`: a launch that
  quietly repaired topology would hide exactly the state the refusal exists
  to surface.
- **Selection fails closed.** A name nothing answers to, or two accounts
  answer to, refuses with an actionable message; an empty one is a caller's
  expansion bug and the command line refuses it. The old shell fallback
  turned an unresolvable choice into "launch the primary with permissions
  bypassed", which made corruption indistinguishable from a choice; no
  surface mints a primary decision nobody made — a resume without an owner
  to follow asks the rule, never falls to the primary.
- **A neutralized inherited value is visible when it cannot be explained,
  never silently obeyed.** Every managed launch strips the ambient variable
  regardless; what varies is the reporting. A value naming a discovered
  extra account's dir is what every shell *inside* a managed session
  inherits — this machine's ordinary environment all day — and a notice
  that fires on the ordinary case is noise, so the launch's notice stays
  quiet for it (the discovered set's `KnownDir`, beside the target's own
  conflict classifier so diagnostics and the environment built cannot
  disagree). What stays loud, because tools
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
`CLAUDE_CONFIG_DIR` and hold no verification a
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
frozen side gets names and personal flags. The same asymmetry decides how protocols
may evolve: a version tag inside a line only helps a reader new enough to
check it, so a breaking change to a wrapper-facing surface ships as a
*renamed verb* — argv is the one channel where the binary can observe a
stale caller and refuse it, actionably. (An unchanged surface may keep an
old spelling, but a surface whose contract changed must never reuse its
name.)

## Automatic placement

A person with several subscriptions would otherwise spread sessions by hand, a
named launcher per session. A bare launch chooses the account itself, every
time: there is no mode to turn on and nothing a shell wrapper passes, since
shell functions are frozen at shell init and panes would disagree about what
`x` does for weeks. Naming an account is the one way to overrule the rule,
and it lasts one launch.

- **Placement is launch-time and nothing else.** A session runs where it was
  started; no request is proxied and no running session is moved. The promise
  is that new sessions spread. A session can still exhaust the account it was
  placed on.
- **Each limit does one job.** `internal/placement` is one pure function. The
  session window — the vendor's shortest limit, five hours for Claude Code —
  ranks accounts as *load*: its usage in steps of ten points, plus one step
  per busy session and one per launch of the last fifteen minutes — on that
  subscription, a headless caller's included. Any limit
  at 80% or above sets an account aside while another has room. Weekly room
  breaks ties, then the least recently placed account. The first draft of
  this rule folded both windows into one figure ("the most room on the
  tightest limit"), and with session usage near zero the weekly figure
  absorbed every increment a launch added: four launches in a row went to one
  account. Counting usage in steps, and busy sessions in the same unit, keeps
  a one-point difference from outranking forty points of weekly room, and an
  account at 9% with five busy sessions from beating one at 11% with none.
  The constants are policy, not measurement. The vendor may leave the session
  window out of a response, which still parses: such an account is ranked on
  its busy sessions and recent launches alone, weekly room then decides most
  launches, and `check` counts the fresh responses that carry the window so
  that its absence is seen.
- **Weekly room matters as much as the five-hour window.** Over thirty days of
  transcripts on the author's machine, messages about the model-scoped weekly
  limit appeared on four days and messages about the five-hour limit on one.
  That counts messages, not lost work, and it is enough to say the weekly
  limit is not the rare case. So a weekly limit can take an account out of
  the running, and decides every tie — but it never hides a five-hour
  difference between two others.
- **Weekly room is spent in the order it lapses.** What is unspent at a
  reset is gone, so a tie goes to the most room per second its window has
  left: room that ends tomorrow before room that lasts the week. Lowest
  percent first evens out current usage and holds expiring room back behind
  room with days to run; ranking by the reset first piles sessions onto one
  account, so the reset orders ties and nothing else. An account is judged
  by its tightest weekly row, the least room per second: judging its highest
  percent first lets one more point spent on a row about to reset make the
  account more attractive. Launches started together still spread, each one
  load on its account, while one session at a time returns to the room that
  lapses first once the last launch stops counting — the preference, not a
  pile-up. The gain shows only where weekly demand nears what is left; with
  room to spare, which account takes a tie changes nothing.
- **A figure counts only for the window it describes.** An observation older
  than fifteen minutes is stale and its rows are lower bounds; a row whose
  reset has passed counts as zero, because that window has ended. The board
  shows the same row as unknown, and both are right: a display must not print
  a low percent for a window that is over, while a placement may use it as a
  bound because it says so and counts its own launch against it. The time
  such a weekly window has left is the next renewal its schedule names:
  Claude Code renews weekly windows every seven days at a fixed instant,
  spent or not (`usage.Period`) — observed on every reset the owner's
  readings held, not asserted by `check` — and an idle account's figures can
  be days old, so counting its renewed week whole from now would rank it
  behind room that in fact lasts longer. Codex windows start with the next
  request, so a passed or unstarted one counts its whole stated window, and
  a window nothing dates counts a whole week, the least pressing it can be.
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
  a locked Keychain every account reads that way, and for an automatic
  launch every usable account is a correct answer. For the same reason missing bookkeeping
  never refuses an ordinary launch: a busy lock or a newer schema costs the
  launch its record and a line on stderr, and a placements section that does
  not decode is set aside and begun again by the launch's own placement,
  like the request ledger. A damaged `.reserved` still refuses, since it
  says nothing about which accounts the person kept for what.
- **A reservation says what an account may be used for, never that it goes
  first.** A subscription bought for one job is kept to it by `accounts
  reserve <account> <dir>...`. Elsewhere an automatic choice cannot take the
  account: it is an ordinary exclusion, built with the rest (`keptOut`), for a
  launch, the board's mark and a headless placement alike. In those
  repositories it is ranked like any other: the rule never receives a
  reservation, so it cannot prefer an account for what it is kept for, never
  reads a directory, and every other account stays available there. The obvious second
  half, preferring the account in its own repositories, is what the design
  declines: going first until 80% overruns a small plan's five-hour window in
  a model of the owner's launches, because sessions already running keep
  spending after the gate closes, and winning ties would give the rule a
  second job for a modest gain. A plan is no input either: percentages are
  relative to each account's own quota, and weighting by plan needs a ratio
  nobody has measured. A launch's place is the directory
  it starts in, a session's own on a resume, a headless caller's working
  directory, resolved by `sessions.RepoKey`: the main checkout, whichever
  worktree it is reached through, canonicalized first, since a symlink into a
  worktree has no `.git` above its spelling. Paths compare by component, so
  `~/dev/itell-cms` is not inside `~/dev/itell`. What fails closes: a directory
  gone leaves its account launchable only by name, because a missing path is
  not permission, and a `.reserved` that cannot be read refuses every automatic
  choice and leaves a launch that names its account alone. Naming is the
  person's word and is obeyed anywhere, but resuming a session is not naming
  an account: outside the repositories a resume moves off the account and
  records the move.
- **The board's mark and the launch are one judgment.** `← next` and a launch
  build their candidates in one place (`launchop`'s `Mark` and `Gather`), from
  the same credential reader and the same exclusions, so the row the board marks is
  the row a launch would take from those figures. A builder of the board's
  own can share the rule and still disagree on who is eligible, and then the
  mark names an account a launch refuses. The mark stays advice — a launch
  reads the disk again — but it is never a second opinion. An open board
  marks again at every frame's time from what its last round gathered: the
  clock alone moves the choice — a window ending, a launch ageing out of the
  load, a weekly reset coming nearer — and rounds stop once nobody is at the
  keys, so a mark decided only per round drifts from the launch with nothing
  re-read. A launch from another terminal shows at the next round. A
  reservation makes the choice depend on the directory, so the mark speaks for
  a launch from where the board was opened. The same counting
  gives every row its load on the board and in `--json` (schema 8): busy
  sessions, a headless caller's running sessions among them, and recent
  launches.
- **Choosing and recording are one store operation.** `state.Place` reads the
  recent launches, calls the rule and records the result inside one locked
  section, which is `Claim`'s shape applied to launches: two launches started
  together cannot both see an account as empty. The rule is imported and
  called by the store, so no caller hands a function into the lock. Every
  launch that can start goes through it, a named one included, because a
  session started by name is load an automatic one must count.
- **A launch is one operation.** `headroom launch` builds a request and
  commits it through `launchop`'s plan, which, for an automatic launch,
  decides the intent from a session the launch names in its arguments; a
  named launch reads nothing there and records no re-home. It refuses first
  whatever is known before a write — a named account that cannot be
  launched, a reservation that cannot be read — since `state.Place` prunes
  even when its rule refuses, and a launch that cannot start must leave
  nothing behind; then it places and logs, and the surface announces the
  outcome and execs.
  What the launch means for a session travels into `state.Place` with it, so
  the load and the session's re-home are one write: both land or neither
  does. What a launch owes the next one is decided there as well: a launch
  the rule placed leaves the refresh behind it, and a launch that records a
  session sweeps the re-homes whose transcripts are gone, so an agent that
  names a new session id on every run never grows the file. A surface keeps
  its terminal, its working directory and the exec.
- **Every launch says where it went, and what its figures rest on.** One line
  on stderr before the vendor starts, automatic or named, because a line that
  appears on some paths teaches nothing about the others. A figure from an old observation is
  printed as a bound (`≥12%`), a window whose reset has passed as
  `window ended`, a percent that does not parse as `?%`: the rule counts
  each of those differently from a measured figure, and the line must not
  print them as one. The weekly figure carries the time it has left
  (`week 7%, resets in 1.9d`), since that decides ties; the dry-run table
  prints a renewal projected from a schedule as `≈6.5d`, never as the
  vendor's countdown.
- **A launch counts for a fixed span.** Fifteen minutes, whether or not its
  process is still alive and whatever was observed since: a short job that has
  exited still spent what the figures have not caught up with, and an
  observation taken a second after a launch reflects none of it. Once the
  vendor's registry reports that same process as busy it is counted once.
  The span is one rule (`placement.Pending.Counts`) for the load, the prune
  and a headless caller's `started`, which finds no placement past it
  whether or not anything has pruned the record.
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
Codex places its launches the same way, with what it has:
the shortest window of its main rate limit as the session window, its
allowance as the block, and launches as the only load. It has no registry to
read and no session ownership, so `resume` is placed like a new session.

### The launch log

`launches.jsonl`, beside each vendor's `state.json`, holds one line per
launch and per headless placement: when, the vendor, how the account was
decided (`auto`, `named`, `headless`) and the rule's word for its choice,
the account, the working directory, the session the launch named, the pid
and any bookkeeping that failed. A choice the rule made also carries what it
rested on, derived from the decision: `age_s`, the age of the chosen
account's figures; `tied`, the accounts at its load, so past one the
tie-break decided; `excluded`, the accounts it could not consider. It explains and no code reads it — a person
reads it with `jq` — so it is appended with `O_APPEND` and never rewritten,
takes no lock, and a line that cannot be written is a note on the launch,
never a refusal. Deleting it or tearing its last line changes nothing
headroom does. It is unbounded: at the busiest machine's rate it grows a few
megabytes a year, and the questions it answers — which launches went where,
which surfaces are used at all, how often the tie-break decides — need weeks
of it. Lines under an earlier `v` carry other fields and lack those three;
every version has the rest. The rule's
constants are changed by reading this log and the board, and a change to
what the rule chooses is held by the placement tests, never by a replay.

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
removal refusal, Claude Code-only login and headless tokens, each vendor's
group of checks.

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
  `~/.codex`); logging in under another home leaves the first's login
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
  `~/.codex/sessions`, another home resumes the first's sessions, builds its
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
- **No session ownership.** Resuming across accounts is the vendor's own
  `resume` over the shared store, so no owner evidence or re-home exists for
  Codex, and a Codex resume is placed like a new session.
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

## The code

The vendor set is closed at `claude` and `codex`. `config.Load` resolves one
`Scope` per vendor — paths, binary, usage URL, request spacing, launcher
format, environment policy, presence — every path derived from one home
(`HEADROOM_HOME`, else `$HOME`). `accounts.Discover(scope)` returns a `Set` that owns `Select`,
`Has` and `KnownDir`; an `Account` carries its scope, so no operation
takes a scope beside an account. The set travels with whatever was built from
it — a round, the page that shows it, the disk snapshot — and is never rebuilt
from a scope plus a list. Each vendor keeps its own `.order`, `.reserved`
and `state.json` under its own accounts root.

- **The command line** is `cmd/headroom`: Kong structs on clikit
  (`github.com/qiushiyan/clikit`), one declaration per command holding its
  flags, constraints and `Help()`, so the help page is generated from what
  parses. Configuration loads only when a command asks for it, so help
  prints whatever the environment says. A command's `Run` turns its flags
  into one typed call into `internal/app` and maps the returned error to a
  status. How help renders, how an argv error reads and which errors are usage
  errors are clikit's: a change there is a clikit release and a `go.mod` bump,
  reviewed through the golden help pages. One input bypasses the grammar:
  `main` hands the `$BROWSER` call to `app.Open` before argv is parsed (see
  Renewing logins).
- **Surfaces** are `internal/app`: one typed entry per surface — `Board`,
  `Launch`, `Login`, `HeadlessPlace`, `AccountsRemove`, … — each
  taking its streams (`Streams`), the terminal facts it reads (`Terminal`,
  stdin and stdout apart: the board and remove's y/N need both, mint only
  stdin) and the
  edges it crosses as values: `launchop.Deps` (credential reader, process
  probe, detached refresh), `round.Deps` (credential reader, health probe),
  an `ExecFunc`. No package-level seam and no write to the process's own
  streams; `cmd/headroom`'s `host` binds the live edges once, and a test binds
  its own.
- **Account facts and live requests have separate owners.** `accountstate`
  assembles discovery, the newest usable disk observation and a Codex
  account's plan; it owns `Health`, `Observation`
  and `Attempt`, and its read has no credential, probe or HTTP dependency.
  `refresh` owns credential and identity eligibility, durable request
  claims, parallel fetches, response interpretation and completion
  recording. A candidate binds its ledger key, vendor, prepared request
  and expected identity behind unexported fields, and `Start` refuses a
  candidate of the other vendor. `state` owns the request ledger, stored
  bodies, placements, headless runs and re-homes, one document under one
  lock. `render` decides an account's notes once (`clauses`) and lays them
  out in the block and the compact row; JSON serialization stays with the
  surfaces.
- **A round** is `internal/round`: one vendor's discovery, facts on disk,
  access (health and a request candidate — the second place vendor
  documents are read), claim, fetches and results. `Prepare` is the local
  half, `Fetch` the budget half; each result carries its round, so `Apply`
  ignores another round's. `FromDisk` is `limits`' read, with no request on
  any row. The board is a page per present vendor (`app/page.go`), holding
  its round, selection, schedule and result channel until the channel closes
  and drains, so a result lands on the page that started it and only the
  visible page starts rounds; `picker.step` (`app/picker.go`) is the one place
  results are routed and rounds started. A page's schedule moves by its
  events alone — a round started, the `r` key, the loop's second, a result
  or its channel closing — on one clock, with the channel the only record
  of a round in flight. Selection survives rediscovery by
  account name. Off a terminal the board prints one frame (`app/board.go`);
  `--json` serializes the same rounds.
- **Sessions.** `internal/sessions` reads what a launch weighs about
  sessions: the live registry and its claims' verification (`InspectClaims`,
  one process sample per pid), prompt history and the owner rule over it
  (`OwnerOf`), the transcript ids the re-home sweep consults, and the
  repository a directory belongs to (`RepoKey`). It reads no transcript
  content.
- **Placement.** `placement` is the rule: one pure function over candidates,
  recent launches and an intent, with no clock, file or network of its own.
  `internal/launchop` owns a placement from request to record. `Gather`
  observes what every placement weighs once — stored observations, the
  accounts' session registries and the headless runs (one inspection of
  every claim, one process sample per pid, owner evidence from the
  accounts' own claims alone) and, when the rule will
  choose, credentials — and builds the candidates. The `Plan` it returns
  previews (the dry run, through the same refusals) or commits: preflight
  refuses what is known before any write, then `state.Place` (the placement and a session's re-home in one locked
  section), the launch log line (`log.go`), and for an automatic choice the
  detached refresh. `headroom launch` commits through it, keeping only its
  terminal, working directory, announcement and exec.
  `Mark` is the same gathering for the board's `← next` and load figures, and
  `PlaceHeadless` and `Started` are the protocol for a headless caller, with
  its own eligibility and no exec. `accounts.ReadReservations` is the one
  reader of `.reserved`.
- **Login.** `app/login.go` chooses accounts on the board's health evidence,
  builds `claude auth login` under `launch.Prepare`'s environment with the
  binary itself as `$BROWSER` (`loginCommand`, for the batch and the holder
  alike; `app/approval.go` owns that opener for `login` and `headless
  mint`), relays only a pasted code carrying the sign-in's state, and
  decides whether a login took (`judgeLogin`, shared with the handoff);
  `internal/browser` matches an email to a Chrome profile and reads the
  vendor's sign-in URL. `app/login_handoff.go` is `start`, `finish`,
  `status` and `cancel`, `app/login_holder.go` the holder's life;
  `internal/signin` owns a dir's sign-in files — the login lock, the start
  lock, the record and its steps (`Signin`), the vendor registration — the
  vendor login's life under
  that registration (started, registered or ended, waited for, stopped),
  for the batch and the holder alike, and the socket protocol, knowing
  nothing of accounts. See Renewing logins.
- **Launch preparation.** `launch.Prepare` validates routing, topology and
  the executable and builds the environment before any surface persists a
  choice. `internal/tui` owns raw-mode lifetime, restoration on
  SIGTERM/SIGHUP/SIGINT (restore first, then re-raise so the exit status stays
  honest about the signal) and incremental key decoding — escape sequences
  may split across reads, and a bare ESC resolves as a keypress only after a
  pause. Surfaces own keymaps and frame policy and emit rows through one
  `writeRows` (`app/frame.go`).

Dependencies: the standard library, `golang.org/x/term` (raw-mode terminal
control for the board), and clikit with Kong for the command line — no TUI
framework, no cgo. Every drawn line is one physical row: vendor text —
account labels, model names — is sanitized where it is rendered, and a
screen's last
row carries no newline, since one more would scroll a frame of exactly the
screen's height by a row on every draw. The Keychain is read by exec'ing
`security(1)`, which keeps builds trivial. Parsing is pure functions
(`[]byte` in, tagged structs out) tested by table; exec and HTTP stay in thin
edges.

## Verification

`make check` runs vet, `go fix -diff` (which must print nothing), the
race-enabled Go suite and gofmt. The suite is hermetic: the `TestMain` of
`internal/app` and of `cmd/headroom` clears every variable a vendor's
environment policy strips, every `HEADROOM_*` override, `BROWSER` and the ssh
markers, because the shell running the suite is often a managed session and
each of them changes what a launch prints and where it routes; a test that
needs one sets it with `t.Setenv`.

The command line is held at three tiers in `cmd/headroom`. `TestContracts`
runs the built binary over a fixture home with stub vendors and no terminal,
in the shapes other repositories invoke it, and compares every stream, exit
status and vendor argv with `testdata/contract/`; a pin changes only with
what its consumers read (`HEADROOM_CONTRACT_UPDATE=1` rewrites them, apart
from the help pages so that accepting one can never rewrite the other).
`grammar_test.go` runs the App in process: every argv refusal exits 2 having
started nothing and changed nothing in the home — a retired verb or flag
among them — what argv decodes to, help and a retired verb's refusal under a
configuration headroom refuses, and a missing vendor. `TestHelpPages` pins every help page (`make golden` accepts a
change). Surfaces below it are tested through their typed entries, `round`
and `launchop` through their own interfaces.

Parser tables pin vendor contracts and visible drift. The placement rule is a table over candidates;
the store's place operation is tested for the same property as the claim —
contending handles on the real lock — and the launch path through the exec
seam with credentials and the process table injected. The detached refresh
is the exception: it is exercised through the built binary against a local
endpoint that stalls, since the property is that the launch returns first
and a process that outlived it records the answer. The launch log is held by
the launch tests: one line per launch and placement, an unwritable log a
note and never a refusal, and a deleted or torn log changing no choice.
Operation tests exercise request preparation,
local HTTP and the real store together, so sent credentials, received facts,
refusal backoff and persistence are tested through the callers' interface.
Checker tests use fixture processes and HTTP to verify the final exit verdict.
Store tests own locking, generations, carrying sections they cannot read, and cooldowns;
launch tests own routing, refusal-before-persistence and failure-after-write.
`login` is tested through its command with the vendor's login injected —
which accounts a window, names and `--all` choose, what is held back, and a
read-back that fails an unchanged credential, an access-token refresh, a
failed vendor command and a login as the wrong email — and through the
built binary against stub `claude`, `open` and `security`: the login's dir,
headroom as `$BROWSER`, the profile `open` receives, nothing opened for a
remote user, the opener answering under a configuration headroom refuses,
and a pasted code reaching the vendor only when it carries the state the
opener recorded. The relay is driven directly with scripted lines: what it
refuses, what it says, the same line twice, and the vendor's input ending
with the terminal's. The handoff is held both in process and through the
built binary, because the process boundary is where a suite run in one
process stayed green over real defects — a vendor that inherited the
holder's lock, a lock closed by the collector. In process, the holder runs in a goroutine over a real socket and
lock with a fake vendor that prints 2.1.311's screen: reuse, refused codes,
one code in all, the deadline against a code in flight, a busy holder, an
unwritable record, the batch excluding a start, a dead holder's exchange
waited out. Through the built binary and a stub vendor that outlives its
input as the real one does: `start` returning through streams read to their
end while its holder lives in a session of its own, a holder whose
collector runs keeping its lock, vendors left by a killed holder or a
killed batch ended by the next `start`, and a vendor that cannot be
registered refused. `security`'s answers are a table over a stub. The profile match and the code's shape are tables, the
first over the author's own `Local State`. `headless` is tested through its
commands over the real store — a placement among token holders, the
refusal near a limit and its retry instant, a run counted once, past
fifteen minutes, then swept — and `mint` through the built binary under
script(1) against a stub `setup-token`: the
scratch dir, gone after; headroom as `$BROWSER`; the token stored 0600 and
never in the output. The scanner is a table over reads cut at every size.

The detached refresh is held through the built binary too, under a
launchd-sized environment: one killed mid-request leaves the store readable,
unlocked and askable after one spacing, and one sent SIGTERM — mid-request or
mid credential read — stops within a second, having claimed nothing it could
not complete.

`make test-pty` (`test/pty/`) covers what Go tests cannot observe: the
board's interaction, selection, refresh scheduling, scrollback and terminal
lifetime, read through a real pty. Automatic placement is exercised there
through the real binary: five launches started together reach five
accounts, each one line in the launch log, a session named on its first turn
resumes on the same account, the board marks one account's row, and no key
on the board writes routing. Each retired verb exits 2 with nothing on
stdout. The harness sets `HEADROOM_HOME`, from which both accounts roots
derive, and points both usage URLs at a closed port. Codex's fixture root
does not exist until its block creates it — so every frame before that is
the single-vendor one — and Codex is present for one block that pins the
two-vendor print, tab switching, and keys on the Codex page writing nothing.
Signals must restore the terminal before exit. The harness uses fixture
`claude` and `security` commands and redirects account and session state
into a temporary home. Signal cases address the recorded fixture child PID.
An outer shell checks terminal state after exit; the tmux scrollback case
skips when tmux is absent. The dotfiles repo's
`zsh/.config/zsh/tests/claude-launch.test.zsh` tests its shell wrappers
against this checkout's binary and a recording `claude` stub.

## Status

Personal-first: the defaults fit the author's machine, and `HEADROOM_*`
environment variables re-point each of them. The endpoints headroom reads are
undocumented and restricted by the vendors' consumer terms to first-party
clients; this repository exists as a personal, read-only instrument, not as
a distribution.
