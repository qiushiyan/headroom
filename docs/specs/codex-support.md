# Codex support — a second vendor on the board and the launcher

Status: built on the `codex-support` branch, 2026-09-21; kept as the dated
record of the decision. The design as it stands lives in `DESIGN.md` § A
second vendor: Codex — read that, not this, for what is true today. Written
against `main` at 8c00fdf, codex-cli 0.155.0, macOS.

## Summary

Current: headroom shows quota for, and launches, the Claude Code accounts on one machine.
Current: every package assumes one vendor; paths, the usage document, credentials and the launch environment are Claude Code's.
Current: the owner also holds two OpenAI Codex CLI accounts and switches between them by hand.

Goal: the board gains a Codex page beside the Claude Code page, switched by tab, with the same three-axis honesty; enter records the chosen account per vendor.
Goal: `headroom launch --vendor codex` execs `codex` on a chosen account, with the environment built from that decision alone.
Goal: `headroom accounts add --vendor codex` seeds a Codex home that shares one session store, so Codex's own `codex resume` reaches any session from any account.

Change: configuration resolves one scope per vendor, and an account carries its scope.
Change: shared mechanisms (discovery, `.current`, topology, the claim ledger, refresh, facts, rendering, launch preparation) take the scope; vendor documents get their own leaf parsers.
Change: each vendor keeps its own `.current`, `.order` and `state.json` under its own accounts root; no existing file is re-keyed.
Change: the interactive board's state moves from one picker to one page per vendor, each owning its selection and its refresh round.
Change: a limit row gains a window duration, a feature and an unstarted flag, and an observation gains an account-level allowance.
Change: `--json` becomes schema 5: a vendor on every account, and top-level `current` changes from a string to an object keyed by vendor.

Boundary: `headroom sessions` stays Claude Code only, and `accounts remove` refuses Codex accounts.
Boundary: Codex usage is never read from Codex's session files; before headroom's first fetch a Codex account's usage is unknown.
Boundary: only Codex's file credential store is supported; a keyring login reads as not logged in, and `check` says why.
Risk: the Codex usage endpoint's request budget is unmeasured; spacing is a per-vendor policy value, and every ledger deadline is made to respect it (§ Design — Premises, P7).
Unverified, each with a fallback that keeps the design: the unstarted-window rule (measured once), the request spacing (assumed), and the secondary, additional and code-review window shapes (from the vendor's source, never seen live).
Open: no decision waits on the owner.

Where: § Behaviour describes the eight situations; § Design carries the scope seam, the row and allowance shapes, the wiring and the vendor evidence; § Verification numbers the obligations; § Delivery holds the phases.

## Intent

### Goals

- A person with Codex installed opens `headroom accounts` and sees a Claude
  Code page and a Codex page. Tab switches pages. Enter records the selected
  account as the target of that vendor's bare launch.
- Codex accounts are described by the same three independent axes as Claude
  Code accounts: health, observation, attempt. Figures never appear without
  their age. A failed request never reads as a failed account.
- `headroom launch --vendor codex [--account <name>] [--remember] -- <args>`
  execs `codex` on the chosen account. `headroom resolve --vendor codex`
  answers the same three columns it answers for Claude Code.
- `headroom accounts add --vendor codex <email>` creates the home, links its
  `sessions/` to the canonical Codex session store, and verifies what it
  built with the topology check launch applies. It works on a machine where
  no Codex directory exists yet.
- `headroom check` verifies the Codex facts the above rests on, with the
  same PASS / FAIL / INCONCLUSIVE discipline.
- A machine without Codex behaves as it does today.

### Non-goals

- **Codex sessions in `headroom sessions`.** The owner resumes across
  accounts by naming the account at launch. A shared `sessions/` store makes
  Codex's own `codex resume` see every session from every home (§ Design —
  Premises, P5). No Codex session reader, ownership model or re-home is
  built.
- **`accounts remove` for Codex.** Claude Code's removal is gated on a live
  session registry. Codex has none that headroom can read. An empty read
  must not pass as proof of inactivity, so removal refuses and names the
  directory to delete by hand. It ships when liveness evidence exists
  (`thread-writer-locks/` is uninvestigated).
- **Usage from Codex rollout files.** Rollouts carry per-turn `rate_limits`
  records but no account identity, and under a shared store one rollout holds
  turns from several accounts (P5). Attributing a snapshot would show one
  account's quota under another's name.
- **Keyring credentials.** Codex's `cli_auth_credentials_store = "keyring"`
  puts the login where headroom cannot read it without a Keychain prompt.
  The board reads such a home as not logged in. `check` reports a
  disagreement between `codex login status` and `auth.json` as an
  undetermined credential source; it does not claim to detect keyring mode
  in every case (a keyring login beside a stale `auth.json` is invisible).
- **Non-ChatGPT logins** (API key, agent identity, Bedrock). They have no
  subscription window. They render with health unknown and a caption naming
  the auth mode. No request is made.
- **Credits, reset credits and per-model availability as figures.** The
  parser keeps what changes whether the account is usable (§ Design — API).
  Balances and counts are not rendered in this version.
- **Managing the Codex desktop app.** It shares `~/.codex`. headroom never
  writes that home's `auth.json`, so the app's login is not disturbed.
- **The owner's shell wrappers.** A `cx`-style function lives in the owner's
  dotfiles, as `x` does.

### Vocabulary

**Vendor** is one of two fixed values: `claude` or `codex`. **Scope** is the
resolved configuration for one vendor. **Home** is a Codex state directory,
the thing `CODEX_HOME` selects; it plays the role Claude Code's config dir
plays. **Primary** keeps its meaning per vendor: the vendor's default
directory, selected by the absence of the vendor's environment variable.
A Codex **window** is one `*_window` object in the usage response. It maps
onto the existing limit row; it is not a new concept.

## Tenets

1. A shared invariant has one owner, written once and taking the scope;
   vendor evidence and the actions a vendor supports stay explicit. A second
   spelling of claims, launch rules or freshness drifts from the first. A
   vendor interface with a method per stage pushes document knowledge into
   callers. Held by: § Design — Structure (three document-reading dispatch
   points) and obligation 1.
2. Identity travels with every observation and every decision. A shared
   directory, a matching email or a recent timestamp never establishes whose
   quota was measured. Held by: one auth snapshot per account per round, the
   two-part ledger key, the response identity check and distinct accounts
   roots (§ Design — API, § Design — Wiring) and obligations 2, 5, 7 and 8.
3. Destructive authority and account-failure claims require positive
   evidence. An empty liveness read is not inactivity. A rejected request is
   not a failed account. Held by: obligations 7, 11 and 14.
4. Shared presentation normalizes meaning without inventing vendor facts. A
   Codex window is never given a Claude Code limit name. A field that
   changes whether the account is usable never silently disappears. Held by:
   the row and allowance shapes (§ Design — API) and obligations 4 and 6.
5. The validated account decision owns the effective launch. Ambient
   credentials and redirects are inside the one environment constructor, or
   they reopen routing. Held by: § Design — API (environment policy) and
   obligation 9.
6. A new vendor adds files; it never re-keys an existing one. Selections,
   re-homes and quiet periods are human decisions and spent budget, and an
   upgrade must not reset them. Held by: one store per accounts root
   (§ Design — Structure) and obligation 1.

Unless you know better ones.

## Behaviour

### 1. Opening the board with both vendors present

Today: the board lists Claude Code accounts.

After: the board opens on the Claude Code page. A one-line tab bar above the
header names both pages and marks the visible one. Tab switches pages. Each
page keeps its own selection, scroll position and refresh schedule. `j`/`k`,
`r`, enter and the cancel keys act on the visible page. Enter on the Codex
page records the Codex current account and prints which bare launch now
targets it. It does not touch Claude Code's `.current`.

Mechanism: one page per present scope, each owning its list and its
in-flight refresh round (§ Design — Wiring, board).

Only the visible page starts refresh rounds. A round already claimed when
the person switches away finishes and updates the page that started it.
A hidden page keeps its figures and their age.

### 2. A machine without Codex

After: when neither `~/.codex` nor the Codex accounts root exists, there is
no tab bar and the frame is what it is today. `--json` reports schema 5 with
Claude Code accounts only. `check` prints one line saying Codex was not
found. `launch`, `resolve`, `limits` and `accounts remove` with `--vendor
codex` fail with that reason. `accounts add --vendor codex` is the
exception: it creates the accounts root and the canonical store, as seeding
already does for Claude Code.

### 3. A Codex account headroom has never fetched

After: the row shows the account, its plan and its health, and says usage is
unknown. It does not borrow figures from Codex's session files. The first
permitted fetch fills it, and later runs inside the quiet period replay that
stored response with its age.

### 4. A Codex account that has never been used

Observed: the vendor reports a window with 0% used whose reset is a full
window away and slides forward with each request until first use (P4).

After: the row shows 0% and the words "not started" where the reset
countdown would be. `--json` reports `unstarted: true`, `reset_state:
"none"` and a null `resets_at`.

If the rule misses an unstarted window, the row shows a countdown of one
full window, which is harmless. If it fires on a started window, a real
reset is hidden behind "not started"; the predicate is kept narrow for that
reason (§ Design — Premises, P4).

### 5. A Codex account the vendor says is blocked

After: when the response carries positive account-wide blocking evidence
(the allowance table in § Design — API), the account is not offered as
grounds for a choice, whatever its percentages say. The row carries a
caption naming the reason. The not-actionable warning counts it. A block
that applies to one feature only (code review, an additional limit) is
named in a caption and does not make the account unavailable.

Mechanism: the observation's allowance, consulted by `Actionable`
(§ Design — API).

### 6. A request that fails

After: a 429 defers the account and records backoff, as today. A 401 is an
attempt outcome: the rows and health stay as they were. The caption says the
access token was rejected and that any Codex session refreshes it. Codex
itself recovers from a 401 by reloading and refreshing, so a 401 is not
evidence that a person must log in.

### 7. Launching Codex on a chosen account

After: `headroom launch --vendor codex --account <name> -- <args>` execs
`codex <args>` with `CODEX_HOME` set for an extra account and absent for the
primary. Inherited `CODEX_HOME`, `CODEX_SQLITE_HOME`, `CODEX_API_KEY` and
`CODEX_ACCESS_TOKEN` are removed. Each removal that cannot be explained is
said on stderr. The two credential variables are named without their
values. Claude Code's variables are left alone.

Launch refuses, before writing `.current`, when the account's `sessions/` is
not the shared link, and for the primary when `HEADROOM_HOME` re-points the
home.

Resuming across accounts is `headroom launch --vendor codex --account <other>
-- resume`. Codex's own picker lists the sessions of the current directory;
`-- resume --all` lists every project's. Both read the shared store.

### 8. Removing a Codex account

After: `headroom accounts remove --vendor codex <name>` refuses. The message
says headroom cannot tell whether a Codex session is running on that home,
names the directory, and says to delete it by hand once no `codex` process
runs.

## Design

The existing code is a base this extends, after one opening reshape:
vendor-scoped configuration moves from `config.Config` onto a scope value,
and an account carries its scope. That reshape changes no Claude Code
behaviour and lands first (§ Delivery).

### Structure — responsibilities

- **Scope resolution.** Owner: the config package. Protects: every
  directory that crosses into a launch is absolute, established at the one
  door, and a scope is only ever constructed there. Today `config.Load`
  builds one `Config` whose fields and methods spell Claude Code paths
  (`internal/config/config.go`). After: `Load` returns a `Config` holding
  one `Scope` per vendor. A scope holds the vendor, the home root, the
  primary directory, the accounts root, the canonical session store and the
  name of the per-account link to it, the binary name, the usage URL, the
  request spacing, the launcher format, the primary-name pin, the
  environment policy, and whether the vendor is present on this machine.
  Both scopes are always resolved; presence only decides whether the board,
  `--json` and `check` include a vendor unasked. `Load` refuses two accounts
  roots that name the same location, compared after cleaning and, where the
  paths exist, after resolving symlinks. The comparison never rewrites an
  accepted spelling. Distinct roots are what keep the two vendors' `.current`,
  `.order` and `state.json` apart.
- **Discovery, selection, `.current`, `.order`.** Owner: the accounts
  package. Protects: strict resolution; corruption refuses and never
  defaults. Today `Discover`, `Select` and `SetCurrent` take `config.Config`
  and an `Account` needs the config passed back to resolve its own directory
  (`internal/accounts/accounts.go`). After: `Discover` takes a scope and
  returns the discovered set for it. Selection and recording the current
  account are operations on that set, and an `Account` carries the scope it
  was discovered under, so `a.Dir()` needs no second argument. No operation
  accepts a scope beside an account or an account list, so the wrong pairing
  cannot be written.
- **Reading an account's identity.** Owner: one leaf reader per vendor.
  Protects: identity comes from decoded fields of the vendor's own
  document. Today `accounts.ReadMeta` reads `.claude.json`. After: discovery
  dispatches on the vendor to `ReadMeta` or to the Codex auth reader, and
  both fill a vendor-neutral identity on the account: email, vendor account
  id, and whether the document was readable. Claude Code's cached usage
  stays on its `Meta`. For Codex the account also carries the parsed auth
  snapshot discovery took, as it carries `Meta` today. Labels, the ledger
  key, replay and the candidate's credentials in one round all come from
  that one snapshot. Preparation does not read `auth.json` again, so a login
  that changes mid-round cannot put one account's response on another's
  row.
- **The Codex auth document.** Owner: a new leaf package, sketched as
  `internal/codexauth`. Protects: `auth.json` and the JWT payloads inside it
  have one reader, used by discovery, the board, `check` and the 401
  re-read. One file read yields identity, plan, the access token and its
  expiry. JWT payloads are base64url-decoded and JSON-parsed; signatures are
  not verified, because the file is local and the vendor is the judge of
  the token.
- **Usage documents.** Owner: the usage package. Protects: one parser per
  vendor document type, and one dispatch that live interpretation, replay
  and `check` all go through. Today those three each call
  `usage.ParseLimits` directly (`internal/refresh/refresh.go` `interpret`,
  `internal/accountstate/read.go` `newestObservation`,
  `internal/check/check.go`). After: they call one function that takes the
  vendor and the body and returns a reading: the rows, the allowance, and
  what the body says about whose it is (the account id, the user id and the
  plan, each when present), so no caller decodes a body a second time.
- **The request ledger and stored responses.** Owner: the state package.
  Protects: `Claim` is the single authorization for traffic. Today
  `state.Open(accountsRoot)` already scopes the store to one root, and the
  quiet period is a package-level constant read through `spacing()`
  (`internal/state/state.go`). After: a store is opened from a scope, so it
  knows its root, its vendor and its spacing. `state.Key` and its `ID`
  encoding do not change. The store a body was read from says which vendor's
  parser reads it. Every deadline the ledger computes (the quiet period,
  refusal backoff, and the clamps in `NextEligible` and `Claim` that cap a
  deadline at `CooldownMax`) is at least the scope's spacing.
- **Refresh.** Owner: the refresh package. Protects: no retry inside a run;
  the permit, not an earlier flag, lets a request leave. Today a candidate
  holds a key, a dir and a token, and `Start` takes one URL
  (`internal/refresh/refresh.go`). After: a candidate is built by the
  vendor's access reader and binds the key, the prepared request (URL and
  headers), the vendor, and what the 401 re-read needs. Its fields stay
  unexported so the key, the account header and the token cannot be
  mismatched by a caller. `Start` takes the scoped store and refuses a
  candidate of the other vendor.
- **Facts.** Owner: accountstate. Protects: the three axes stay
  independent. After: an observation also carries the allowance, and the
  fresh window follows the scope's spacing rather than one global constant.
  Sketch: the facts carry their fresh window, set at assembly from the
  scope, so `Fresh` and `Actionable` keep their signatures. A Codex row's
  plan is the one headroom's own response named when an observation exists,
  and the auth snapshot's otherwise; a disagreement between the two is not
  surfaced.
- **Launch preparation and the environment.** Owner: the launch package.
  Protects: the child's environment is a total function of the validated
  decision. After: the target is built from the account, and the variables
  it sets and strips come from the scope's environment policy.
- **Topology and seeding.** Owner: the accounts package. After:
  `VerifyTopology` and `Seed` read the canonical store and the link name
  from the scope. `CheckRemovable` and removal stay Claude Code only; the
  Codex refusal is decided before any of it runs.
- **The board.** Owner: `internal/app/accounts.go`. Protects: each refresh
  round owns one list and one channel until that channel closes and drains.
  After: that ownership moves from the picker to a page.
- **`check`.** Owner: the check package. After: a Codex group of checks
  beside the existing ones, sharing the refresh path as today.

Vendor documents are read through three dispatch points and no others:
reading identity at discovery, reading access (health and the candidate) at
preparation, and parsing a usage body. Paths, names, spacing and the
environment policy are data on the scope. What a vendor supports as a
command (the Codex removal refusal, Claude Code only sessions, each
vendor's group of checks) is decided in the command that owns it, in plain
sight, and is not pushed into scope flags.

### Design it twice — the vendor seam

Three shapes were compared for how vendor variation enters the account
pipeline.

- *Constraint: everything common is one algorithm.* A data-only descriptor
  parameterizes every stage. It fits paths, topology and the environment.
  It cannot say how identity, health and credentials are read, so those
  rules leak into each caller behind flags. Rejected: the descriptor looks
  small because its real interface is in the callers.
- *Constraint: each vendor changes independently.* An interface with a
  method per stage, or a parallel `internal/codex` pipeline glued at the app
  layer. Claims, freshness, launch ordering and topology gain two owners.
  Rejected: those are exactly the mechanisms whose single spelling the
  project's rules protect.
- *Constraint: each invariant has one owner.* Scope data for what is only a
  path or a name, and three dispatch points for reading vendor documents.
  **Chosen.** Callers receive evidence and validated decisions and never
  assemble a vendor rule. The cost is that the reshape touches most
  signatures that take `config.Config` today.

### API

**What a caller writes** (names are sketches; the distinctions bind):

```
cfg, err := config.Load()               // refuses aliased accounts roots
scope := cfg.Scope(vendor)              // always resolved
for _, scope := range cfg.Present()     // claude first; board, --json, check
set := accounts.Discover(scope)         // each Account carries scope
a, err := set.Select(selector)
err = set.SetCurrent(a)
prepared, err := launch.Prepare(a, set, os.Environ())
st := state.Open(scope)
updates := refresh.Start(ctx, st, candidates, reread)
reading, err := usage.Parse(scope.Vendor, body)
```

**The limit row.** `usage.Row` keeps its fields and their Claude Code
meaning. It gains `WindowSeconds` (0 when the vendor does not state a
duration, which is every Claude Code row), `Feature`, and `Unstarted`. For a
Codex row the decoded identity is:

| field | Codex value |
| --- | --- |
| `Kind` | the slot: `primary` or `secondary` |
| `Group` | the limit object the window sits in: `rate_limit`, `code_review_rate_limit`, or `additional` |
| `Feature` | `metered_feature` of an `additional_rate_limits` entry; empty otherwise |
| `Model` | always empty |
| `WindowSeconds` | `limit_window_seconds` |

These are the vendor's own words.

*Label.* The label is derived from decoded fields of the same entry and
from nothing else. It names the duration (`weekly`, `5h`, otherwise days or
hours), and then the group decides the rest. A `rate_limit` row adds
nothing. A `code_review_rate_limit` row adds "code review". An `additional`
row adds its `limit_name`, or its `metered_feature` when the name is empty.

*Column identity.* A compact board column is identified by kind, group,
model, feature and window seconds together. Two accounts whose `primary`
slots have different durations therefore get different columns. A window
whose kind or duration cannot be decoded has a bad identity state and is
not given a column, as today.

*Column order.* Pages never share a table, so columns are ranked per
vendor. Codex columns run `rate_limit` first, then
`code_review_rate_limit`, then `additional` by feature. Inside a group the
longer window comes first, because the weekly window is the one that
strands an account for days. Claude Code's ranking is unchanged.

*Severity.* A Codex row's severity is always `normal`. The vendor sends no
severity, and a reached limit is carried by the allowance, not by a word
headroom would have to invent.

The Codex parser separates rejecting the envelope from degrading a field.
The body is unparseable when it is not a JSON object or has no `rate_limit`
key. A `rate_limit` of `null` is an observation of no limits, as an empty
`limits[]` is for Claude Code. A `null` window, a `null` limit object and a
`null` or absent `additional_rate_limits` contribute no rows. Inside a
present window, a wrong-typed `used_percent` is a bad percent, a wrong-typed
or non-positive `limit_window_seconds` is a bad identity, and a wrong-typed
`reset_at` is a bad reset. An `additional_rate_limits` entry that is not an
object, or has no `metered_feature`, yields one row with a bad identity so
`check` fails on it; it is never dropped silently.

**Reset meaning.** The three field states stay three: the project keeps one
degradation vocabulary (`internal/tag/tag.go`) for a percent, a timestamp
and a credential expiry, and this change does not widen it. An unstarted
window is a separate fact about the row.

| the row | entered when | rendered | written by |
| --- | --- | --- | --- |
| reset ok | a parseable instant | countdown; past means rolled over | either parser |
| reset none | the vendor legitimately states no reset | a dash | either parser |
| reset none, `Unstarted` true | Codex: percent, duration, `reset_at` and `reset_after_seconds` all decode, `used_percent` is 0, and `reset_after_seconds` ≥ `limit_window_seconds` | "not started"; `ResetAt` is 0 | the Codex parser |
| reset bad | present but unparseable | `reset?` and a drift marker | either parser |

An unstarted row never counts as rolled over, because its `ResetAt` is 0.

**The allowance.** An observation carries one account-level allowance: a
state, the vendor's reason string when it gives one, and the names of
features blocked on their own. The Codex parser decides the state by the
first row that matches, so positive blocking evidence is never hidden by a
malformed sibling field.

| order | state | entered when | effect |
| --- | --- | --- | --- |
| 1 | blocked | any of these is well-typed and positive: `rate_limit.allowed` is false; `rate_limit.limit_reached` is true; `spend_control.reached` is true; `rate_limit_reached_type` is a non-empty string (kept as the reason; the vendor types it as a closed enum, so an empty string is a malformed value and falls to row 2 — amended in review, 2026-09-21) | `Actionable` is false; caption names the reason |
| 2 | bad | none of the above, and one of those four fields is present under a wrong type | never blocks; drift tag; `check` fails |
| 3 | allowed | `rate_limit.allowed` is true and `limit_reached` is false | none |
| 4 | unknown | anything else, which includes every Claude Code response | none; never blocks |

Codex itself treats `spend_control.reached` and the workspace values of
`rate_limit_reached_type` as hard stops (the `AccountRateLimitsUpdated`
handler in its TUI), which is why they are account-wide here. A
`code_review_rate_limit` or `additional_rate_limits` entry whose own
`allowed` is false or `limit_reached` is true blocks that feature only. Its
name goes in the allowance's feature list and is rendered as a caption even
when the entry has no windows. It never changes the account state. Unknown is
never read as allowed, and an empty window list is never read as unlimited.
Credit balances and reset-credit counts are parsed by nobody in this
version.

**Codex health and eligibility.** There is no vendor probe on the board
path; the auth snapshot decides. Parsing validity, health and request
eligibility are decided in that order, first match wins.

| order | the snapshot | health | attempt |
| --- | --- | --- | --- |
| 1 | file absent | no login | none |
| 2 | unreadable or not a JSON object | bad blob | none |
| 3 | `auth_mode` present and not `chatgpt` | unknown; caption names the mode | none |
| 4 | `auth_mode` is `chatgpt` or absent, and `tokens` is absent, not an object, or has no string `access_token` | bad blob | none |
| 5 | `tokens.account_id` absent, the id token undecodable, its `chatgpt_user_id` absent, or `account_id` disagreeing with the `chatgpt_account_id` claim | ok | identity unknown; no request |
| 6 | the access token's `exp` undecodable or in the past | ok | token stale; no request |
| 7 | otherwise | ok | pending |

An absent `auth_mode` beside ChatGPT tokens is a ChatGPT login: the vendor's
own struct makes the field optional. "Relogin required" is never produced
for Codex: the document carries no refresh-token expiry, and the project
reads expiry only on positive evidence. A Codex account with an unknown
identity has no ledger key at all. The `dir:` fallback that Claude Code
accounts use is not available to it. It makes no claim and replays nothing,
so it renders usage unknown even if an earlier login on that home was
fetched.

**Identity and the ledger key.** The Codex key's identity is
`tokens.account_id` and the `chatgpt_user_id` claim, both required, joined
with a `/` (neither contains one). Two homes on one login share a budget and
two people in one workspace do not. It goes in the existing `Key.UUID`
field, so it is written to disk as `uuid:<account_id>/<user_id>`. That
spelling is permanent once shipped: changing it would orphan every Codex
quiet period. A response is checked against that identity: its `account_id` must
equal the account's, and its `user_id` must equal the user id when the
response carries one. A disagreement is recorded as unparseable and never
shown. A response that names no `account_id` at all is accepted, because
the request was already bound to the account by its header. The same check
runs on replay.

**Environment policy.** Data on the scope, applied by one constructor.

| vendor | sets for an extra | strips always | credential variables (value never printed) |
| --- | --- | --- | --- |
| claude | `CLAUDE_CONFIG_DIR` | `CLAUDE_CONFIG_DIR`, `CLAUDE_SECURESTORAGE_CONFIG_DIR` | — |
| codex | `CODEX_HOME` | `CODEX_HOME`, `CODEX_SQLITE_HOME`, `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN` | `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN` |

A target strips only its own vendor's variables. The primary is selected by
absence. A directory crossing the seam is absolute for both vendors. An
inherited home value naming a discovered extra of the same vendor is the
ordinary "this shell lives inside a managed session" case and stays quiet on
the board and the launch line; `check` still reports it.

**Command surface.** `--vendor <claude|codex>` defaults to `claude` on the
commands that act on one account: `launch`, `resolve`, `accounts add` and
`accounts remove`. Every existing invocation of those means what it meant.
The surfaces that report (`accounts`, `--json` and `limits`, which emit the
same document) show every present vendor, and the flag restricts them to
one. `limits --account <name>` filters by name inside the selected vendors.
`check` takes no `--vendor`: it runs every present vendor's checks, and an
absent Codex is one informational line that does not change the exit code.
The Claude Code scope is always present, as today: a machine without
`~/.claude` still renders the primary as not logged in. Only Codex can be
absent. The
launcher format and primary-name pin for Codex are
`HEADROOM_CODEX_LAUNCHER_FORMAT` (default `headroom launch --vendor codex
--account %s`) and `HEADROOM_CODEX_PRIMARY_NAME`. `HEADROOM_CODEX_ACCOUNTS_ROOT`
(default `~/.codex-accounts`) and `HEADROOM_CODEX_USAGE_URL` re-point the
rest, under the same absoluteness rule. `HEADROOM_HOME` re-points both
vendors' trees.

**`--json` schema 5.** `accounts[]` stays one flat list. Each account gains
`"vendor"`. Top-level `current` becomes an object keyed by vendor, holding
only present vendors. Each limit gains `window_seconds` (omitted when 0),
`feature` (omitted when empty) and `unstarted` (omitted when false). The
three `*_state` fields keep their three values. For a Codex limit
`severity` is always `"normal"`. `usage`
gains `allowance`: `"unknown" | "allowed" | "blocked" | "bad"`, with
`allowance_reason` and `blocked_features` present when non-empty. `source`
keeps `"live"`, `"headroom_cache"` and `"claude_cache"` with their meanings; a
Codex account never reports `claude_cache`. `problems[]` entries gain `"vendor"`.
Under `--vendor`, `accounts`, `current` and `problems` hold the selected
vendor only. The document is always one JSON object. The session document
is unchanged.

**The canonical Codex session store** is the primary home's own
`<home>/.codex/sessions`, as Claude Code's is `<home>/.claude/projects`. It
has no override of its own; it follows `HEADROOM_HOME`. The desktop app
writes the same directory (P5 names what that leaves unestablished).

**Wording for Codex.** Codex has no `/login` command. Every hint that
tells a person to log in, on the board and after `accounts add`, names
`headroom launch --vendor codex --account <name> -- login`, in the engine's
own spelling and not the configured launcher format, because a wrapper's
argument passing is not headroom's to know. The `.order` hint after `accounts
add` carries over unchanged.

**Shared configuration for a Codex home.** `accounts add --vendor codex
--share-config` links a whitelist from `~/.codex`: `config.toml`,
`AGENTS.md`, `themes`, `skills`, `prompts`, `rules`, `plugins`. It is a
whitelist for the reason Claude Code's is: `auth.json`, `history.jsonl`, the
sqlite stores and `sessions/` must not be shared by accident. Nothing
load-bearing rides on the list. `--share-config=<dir>` links every entry
of that directory, as it does for Claude Code.

### Wiring — the changed paths

Account preparation:

- Today: `app.prepare` discovers accounts, loads the store, and runs
  `claude auth status` for every account in parallel through
  `queryHealthParallel`. It injects those answers, the credential reader
  (`creds.ReadRaw`, Keychain with a file fallback) and the clock into
  `prepareWith`, which resolves health and calls `refresh.Prepare` when
  health is ok (`internal/app/app.go`).
- After: the same split. `prepare` gathers what costs a process or a
  Keychain read, per scope, and `prepareWith` stays injectable. It
  dispatches to the vendor's access reader, which returns plan, health and
  either a candidate or the attempt state that blocks one. Claude Code's
  reader is today's body moved. The Codex reader is the eligibility table
  above over the auth snapshot the account already carries; it spawns
  nothing.

Observation selection:

- Today: `accountstate.Assemble` picks the newest of the stored response
  and Claude Code's cached usage, each parsed by `usage.ParseLimits`
  (`internal/accountstate/read.go`).
- After: the same selection, parsed through the one dispatch. A Codex
  account has only the stored response to consider. `limits` still stops
  here. For Codex this path reads `auth.json` for identity, which is a plain
  file read: no Keychain, no probe, no network.

Refresh:

- Today: `refresh.Start` claims the keys, then `fetch` sends a GET with a
  bearer token to the one URL, and `interpret` parses with
  `usage.ParseLimits` (`internal/refresh/refresh.go`,
  `internal/refresh/http.go`).
- After: `fetch` sends the candidate's prepared request. For Codex that is
  `GET <usage URL>` with `Authorization: Bearer <access_token>`,
  `ChatGPT-Account-Id: <account_id>` and a `User-Agent`. `interpret` parses
  through the dispatch and applies the response identity check. On a 401 the
  token re-read uses the vendor's credential reader; its result annotates
  the attempt and nothing else. A failure anywhere leaves the previous
  observation in place.

The board:

- Today: `picker` holds one list, one update channel, the selection, and
  the schedule, and `startRound` replaces the list
  (`internal/app/accounts.go`).
- After: that state is a page, one per present scope, and the picker holds
  the pages, the visible index, and the terminal. The event loop receives
  from every page's channel and resolves each result into the page that
  started the round, by identity of the round and never by an index into
  the visible list. `due()`, `schedule` and `startRound` run for the visible page only, with
  that page's spacing. A hidden page's deadline never wakes the loop; a page
  whose deadline passed while hidden is due when it becomes visible. The tab
  bar is part of the header for the frame's height ranking.
  The one-shot print and `--json` run one round per present scope. When two
  vendors are printed, the text print shows the pages in vendor order under
  a heading line each. With one vendor there is no heading, and the frame is
  today's. `--json` stays one document.

Launch:

- Today: `launch.Prepare` refuses a relocated primary, verifies topology,
  builds the target from the config dir, looks up `claude`, and reports a
  conflicting inherited variable (`internal/launch/prepare.go`);
  `ExecPath` sets `argv[0]` to `claude` (`internal/launch/launch.go`).
- After: the same order, with the binary, `argv[0]`, the stripped set and
  the notices taken from the account's scope. Persistence and exec stay
  with the caller, in today's order.

### Premises

**P1. `CODEX_HOME` isolates a login.** Settled. Basis — measured 2026-09-21,
codex-cli 0.155.0, and established from the `openai/codex` source
tree (`find_codex_home` in its home-dir utility crate). Ran: `codex login status`
with the variable unset, set to an empty directory, to a missing path, and
to a relative path. Result: logged in; "Not logged in" exit 1; hard error;
accepted and canonicalized. Logging in under a second home left the
primary's `auth.json` byte-identical. Does not establish: keyring mode.

**P2. The auth document.** Settled. Basis — measured on two real homes.
Shape: `{auth_mode, OPENAI_API_KEY, tokens:{id_token, access_token,
refresh_token, account_id}, last_refresh}`. `tokens.account_id` equals the
`chatgpt_account_id` claim. The id token carries `email` and, under
`https://api.openai.com/auth`, `chatgpt_plan_type`, `chatgpt_account_id`,
`chatgpt_user_id`. The id token lives one hour and was 190 hours expired on
a working account, so its expiry says nothing about health. The access
token lives ten days. `codex login status` is local (17 ms, works through a
dead proxy) and answers nothing a parse does not; only `check` runs it.

**P3. The usage endpoint.** Settled for shape; the budget is P7. Basis —
established from the `openai/codex` source tree, outside this repository
(backend-client crate: `headers()` in its `client` module and
`rate_limit_status_url` in `rate_limit_resets`), and measured: two calls per account, all 200.
One response, identifiers removed (the real body also carries `account_id`,
`email`, `user_id`):

```json
{"plan_type":"pro",
 "rate_limit":{"allowed":true,"limit_reached":false,
   "primary_window":{"used_percent":93,"limit_window_seconds":604800,
     "reset_after_seconds":259060,"reset_at":1790239759},
   "secondary_window":null},
 "code_review_rate_limit":null,
 "additional_rate_limits":null,
 "model_usage":{"gpt-6-astra":{"available":true,"available_at":null,"credits_would_enable":false}},
 "credits":{"has_credits":false,"unlimited":false,"overage_limit_reached":false,"balance":"0",
   "approx_local_messages":[0,0],"approx_cloud_messages":[0,0]},
 "spend_control":{"reached":false,"individual_limit":null},
 "rate_limit_reached_type":null,"promo":null,
 "rate_limit_reset_credits":{"available_count":1,"applicable_available_count":0}}
```

From source, an `additional_rate_limits` entry is `{limit_name,
metered_feature, rate_limit}` where `rate_limit` has the same shape as the
top-level one. Does not establish: a response with a secondary window or an
additional limit was not observed; those rows are built from the source
structs. The same client exposes a POST that spends a reset credit; headroom
never calls it.

**P4. The unstarted window.** Proposed rule, basis measured once. A
never-used account returned `used_percent: 0`, `reset_after_seconds:
604800`, `reset_at` = request time + 604800. After its first session, a
second call returned a fixed `reset_at` and `reset_after_seconds: 602626`.
Does not establish: that every plan behaves this way, or that the vendor
will keep reporting an unstarted window like this. Fallback: a missed
unstarted window shows a full-window countdown. A false "not started" would
hide a real reset, so the predicate requires every field to decode and the
remaining time to be at least the whole window, which a started window
fails after one second. Either error changes one predicate; the states and
the wiring stand.

**P5. One shared session store.** Settled. Basis — measured in scratch
homes. With only `sessions/` symlinked to another home's, the second home
resumed the first's session by id, built its own sqlite index by backfill,
appended to the same rollout, and later found a session created after its
backfill completed. A session created under one account (plan `pro`,
containing an encrypted reasoning item) resumed correctly under the other
(plan `prolite`); that one rollout now records both plan types and no
account identity. This is why rollout usage snapshots are unattributable.
Verified by the owner on 2026-09-21 through the launcher (`cx resume`, i.e.
`headroom launch --vendor codex -- resume`): Codex's interactive picker lists
the shared sessions and continues one on the other account, which closes the
picker half of obligation 17. Does not establish: the desktop app's tolerance
of rollouts it did not index; `history.jsonl` under a shared store.

**P6. Ambient credentials override the home.** Settled. Basis — established
from the `openai/codex` source tree (login crate, `load_auth` in the auth
manager:
`CODEX_API_KEY` where the caller enables it, which `codex exec` and the main
CLI paths do; `CODEX_ACCESS_TOKEN` unconditionally) and measured: in an
empty home, `CODEX_ACCESS_TOKEN=<junk> codex login status` fails parsing the
junk as a token, so the variable is read. `OPENAI_API_KEY` is read only by
the realtime feature and is left alone.

**P7. Request spacing.** Assumed: 90 seconds, the existing value. No
rate-limit headers were returned and no 429 was provoked. Observed in the code: the
ledger clamps deadlines to `CooldownMax` (16 minutes) in `NextEligible` and
`Claim`, and the board's idle cadence reads the global spacing
(`internal/state/state.go`, `internal/app/accounts.go` `schedule`). A
spacing above those bounds would be shortened today. Fallback: spacing is a
field on the scope, and every deadline and the board's idle cadence take
the scope's spacing as a floor (§ Design — Structure, ledger). With that, a
stricter budget changes a number and the board's cadence, not the shape.
Backoff on a 429 already exists in the ledger.

## Verification

Runners: `make check` and `make test-pty` both ran green from this checkout
on 2026-09-21. The pty harness builds stub binaries under its work
directory (`test/pty/run.sh`); a `codex` stub beside the `claude` stub is
within its existing pattern. Parser fixtures are built from the P2 and P3
shapes, which came from the real writer.

1. Obligation: Claude Code behaviour is unchanged by the reshape.
   Observe: the existing unit suites and pty scenarios, plus one new case: a
   Codex-only refresh leaves Claude Code's `state.json` and `.current`
   byte-identical, and an outstanding Claude Code quiet period still defers
   after the upgrade. A store opened with a spacing above `CooldownMax`
   never yields a deadline sooner than that spacing, after a success and
   after a refusal. Real: the store, discovery, the renderer.
2. Obligation: scope resolution. Observe: `config.Load` under the
   environment overrides; relative values refuse for both vendors; two
   accounts roots naming one location refuse, spelled the same, spelled
   differently, and through a symlink; both scopes resolve when neither
   Codex directory exists, with presence false.
3. Obligation: the Codex auth reader implements the eligibility table.
   Observe: the reader's public result over a table of documents, at least
   one per row, including an absent `auth_mode`, an empty `tokens` object, a
   missing user id, and a document matching two rows to prove the order.
4. Obligation: the Codex usage parser implements the row, reset and
   allowance tables. Observe: parsed readings for the P3 body; a body with a
   secondary window of another duration; an additional limit; a code-review
   limit; the unstarted shape; `allowed: false`; a percent under a wrong
   type; a missing `rate_limit` and a `null` one; `null` windows; a
   malformed additional entry; a non-positive duration;
   `spend_control.reached`; a non-null `rate_limit_reached_type`;
   `allowed: false` beside a malformed `limit_reached`; a feature blocked
   with no windows. Two windows that differ only in duration have different
   column identities. Limit: shapes not observed live are built from the
   source structs.
5. Obligation: one parse dispatch. Observe: a stored Codex body replays
   through `limits` and passes `check`; a Codex body whose `account_id`
   or `user_id` names someone else is not shown; a Claude Code body still
   parses.
6. Obligation: actionability and captions honour the allowance. Observe:
   `Actionable` over facts with each allowance state; unknown and bad never
   block; a feature-only block leaves the account actionable (phase 2) and
   is still rendered as a caption (phase 3).
7. Obligation: refresh for Codex. Observe: through preparation, `refresh.Start`
   against a local HTTP server, and the application of its results to the
   account's facts: both headers present; 200 stores the body in the Codex
   store only; 429 defers with backoff; after a 401 the account's health and
   rows are what they were and the attempt is annotated; no second request
   leaves in one run; `Start` refuses a candidate of the other vendor; an
   `auth.json` rewritten to another login after discovery changes neither
   the row's label nor whose response lands on it within that round.
   Substitute: the HTTP server, which proves the request and interpretation
   and not the vendor's behaviour.
8. Obligation: no rollout-derived usage. Observe: a fixture home whose
   `sessions/` holds rollouts with `rate_limits` records and no stored
   response renders usage unknown.
9. Obligation: the environment constructor implements the policy table.
   Observe: `Env` and the notices for each vendor over inherited values:
   each stripped variable, the primary by absence, the other vendor's
   variables untouched, a known extra's home staying quiet, credential
   values absent from every notice. Launch refuses a relocated primary and a
   broken `sessions/` link before `.current` is written.
10. Obligation: seeding. Observe: `Seed` for Codex builds what
    `VerifyTopology` accepts, on a tree where no Codex directory exists yet
    and on one where the store exists; it refuses a store that is a symlink
    or a file, tells an absent directory from an unreadable one, and links
    only whitelisted config.
11. Obligation: Codex removal refuses and names the directory, with and
    without `--yes`. Observe: the command's exit and stderr; the directory
    still exists.
12. Obligation: pages own their rounds. Observe: at the page level with
    injected result channels, a result arriving after a tab switch updates
    the page that started it; a hidden page starts no round. Through the pty
    harness: tab switches, enter on the Codex page writes only the Codex
    `.current`.
13. Obligation: a machine without Codex renders today's frame. Observe: the
    existing pty board scenarios unchanged; `--json` schema 5 with one
    vendor.
14. Obligation: `check` for Codex. Observe: over a fixture tree with a stub
    `codex`, PASS for a sound tree; FAIL on a drifted auth or usage
    document; INCONCLUSIVE on 429, transport failure, a stale token, and any
    401 (today's Claude Code rule that an unchanged-token 401 fails does not
    carry over, because Codex recovers from 401 by refreshing); a corrupt
    Codex `state.json` reports as headroom's own file. The credential-source
    probe runs `codex login status` once per Codex home, in parallel, each
    with its own `CODEX_HOME`, the vendor's credential variables stripped,
    and a timeout. It reads stderr as well as stdout and tells "not logged
    in" from a command failure. Logged in beside an absent `auth.json` is an
    undetermined credential source, reported INCONCLUSIVE. Not logged in
    beside an `auth.json` the reader accepts as a ChatGPT login is drift,
    reported FAIL. The isolation probe (`CODEX_HOME` set to an empty temp
    dir must answer not logged in) fails loudly otherwise. Limit: the stub
    proves the probes' wiring and verdict mapping; that the installed binary
    honours `CODEX_HOME` is proved only by obligation 17.
15. Obligation: the `--json` schema 5 contract and `--vendor` filtering.
    Observe: the serialized document for a two-vendor fixture from `--json`
    and from `limits`, unfiltered and filtered; `current` is an object; the
    three `*_state` fields never take a fourth value; a Codex limit's
    severity is `normal`.
16. Obligation: Codex presentation rules. Observe: the rendered compact
    header for a Codex fixture orders columns as § Design — API states;
    labels for each group, including an additional limit with an empty
    `limit_name`; every log-in hint for a Codex account names the engine
    launch spelling and never `/login`.
17. Obligation: the real thing works. Observe: on the owner's machine,
    `headroom check` (the isolation and credential-source probes against the
    installed `codex`), the board, one launch per Codex account, and
    `-- resume --all` from the second home listing a session created in the
    first after the second's index was built. Limit: manual, not repeatable
    in CI, and it spends one request per account.

## Delivery

One PR, built as phases on the `codex-support` branch because each later
phase stands on the one before. Two sessions at most.

1. **The reshape.** Scopes in `config`, the discovered set and accounts
   carrying their scope, the store opened from a scope with spacing as a
   floor, and the environment policy as scope data behind the one
   constructor. Codex's scope resolves but nothing reads it yet. The Claude
   Code half of obligation 1, and obligations 2 and 9, green before anything
   else is written.
2. **Reading Codex.** The auth reader, the usage parser, the row, reset and
   allowance extensions, the parse dispatch, the candidate carrying its
   request, and the Codex checks, whose probes use the phase 1 constructor.
   Obligations 3–8 and 14, and the Codex half of obligation 1.
3. **Showing Codex.** Pages, the tab bar, the one-shot print, schema 5,
   `limits --vendor`. Obligations 12, 13, 15 and 16, and the rendering half of 6.
4. **Acting on Codex.** `--vendor` on `launch`, `resolve`, `accounts add`,
   and the removal refusal. Obligations 10 and 11, and the launch refusals of
   obligation 9. Then
   `docs/REFERENCE.md`, `README.md`, `DESIGN.md` (a Codex "system observed"
   section from the premises above), `CLAUDE.md` and both shipped skills in
   the same PR, because command and schema wording is their contract. Then
   obligation 17.

Open decisions: none.
