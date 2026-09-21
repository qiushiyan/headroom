# Codex support — a second vendor on the board and the launcher

Status: proposed, unbuilt. Written 2026-09-21 against `main` at 8c00fdf,
codex-cli 0.155.0, macOS.

## Summary

Current: headroom shows quota for, and launches, the Claude Code accounts on one machine.
Current: every package assumes one vendor; paths, the usage document, credentials and the launch environment are Claude Code's.
Current: the owner also holds two OpenAI Codex CLI accounts and switches between them by hand.

Goal: the board shows Codex accounts beside Claude Code accounts, with the same three-axis honesty, and enter records the chosen account per vendor.
Goal: `headroom launch --vendor codex` execs `codex` on a chosen account, with the environment built from that decision alone.
Goal: `headroom accounts add --vendor codex` seeds a Codex home that shares one session store, so Codex's own `codex resume` continues any session on any account.

Change: configuration resolves one scope per vendor, and an account carries its scope.
Change: shared mechanisms (discovery, `.current`, topology, the claim ledger, refresh, facts, rendering, launch preparation) take the scope; vendor documents get their own leaf parsers.
Change: each vendor keeps its own `.current`, `.order` and `state.json` under its own accounts root; no existing file is re-keyed.
Change: a limit row gains a window duration and an observation gains an account-level allowance; `--json` becomes schema 5 with a vendor on every account.

Boundary: `headroom sessions` stays Claude Code only, and `accounts remove` refuses Codex accounts.
Boundary: Codex usage is never read from Codex's session files; before headroom's first fetch a Codex account's usage is unknown.
Boundary: only Codex's file credential store is supported; a keyring login reads as not logged in, and `check` says why.
Risk: the Codex usage endpoint's request budget is unmeasured; spacing is a per-vendor policy value that can move without changing the design.
Open: nothing.

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
  built with the topology check launch applies.
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
  The board reads such a home as not logged in. `check` names the
  disagreement (§ Verification, obligation 14).
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

1. Mechanisms that own an invariant are written once and take the scope;
   only the reading of vendor documents is per vendor. A second spelling of
   claims, launch rules or freshness drifts from the first. A vendor
   interface with a method per stage pushes document knowledge into callers.
   Held by: § Design — Structure (three dispatch points) and obligation 1.
2. Identity travels with every observation and every decision. A shared
   directory, a matching email or a recent timestamp never establishes whose
   quota was measured. Held by: the ledger key and the response identity
   check (§ Design — Wiring) and obligations 5 and 8.
3. A capability the vendor gives no evidence for is refused, not assumed.
   An empty liveness read is not inactivity. A rejected request is not a
   failed account. Held by: obligations 7 and 11.
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
found. `--vendor codex` commands fail with that reason.

### 3. A Codex account headroom has never fetched

After: the row shows the account, its plan and its health, and says usage is
unknown. It does not borrow figures from Codex's session files. The first
permitted fetch fills it, and later runs inside the quiet period replay that
stored response with its age.

### 4. A Codex account that has never been used

Observed: the vendor reports a window with 0% used whose reset is a full
window away and slides forward with each request until first use (P4).

After: the row shows 0% and the words "not started" where the reset
countdown would be. `--json` reports `reset_state: "unstarted"` and a null
`resets_at`.

If the detection rule is wrong: the row shows a countdown of one full
window. That is harmless, and the rule is one predicate in the Codex parser.

### 5. A Codex account the vendor says is blocked

After: when the response says `allowed: false` or `limit_reached: true`, the
account is not offered as grounds for a choice, whatever its percentages
say. The row carries a caption saying the limit is reached. The
not-actionable warning counts it.

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
-- resume`. Codex's own picker then lists every session in the shared store.

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
- **Discovery, selection, `.current`, `.order`.** Owner: the accounts
  package. Protects: strict resolution; corruption refuses and never
  defaults. Today `Discover`, `Select` and `SetCurrent` take `config.Config`
  and an `Account` needs the config passed back to resolve its own directory
  (`internal/accounts/accounts.go`). After: they take a scope, and an
  `Account` carries the scope it was discovered under, so `a.Dir()` needs no
  second argument. An account cannot be paired with the other vendor's scope
  because no operation accepts the pair.
- **Reading an account's identity.** Owner: one leaf reader per vendor.
  Protects: identity comes from decoded fields of the vendor's own
  document. Today `accounts.ReadMeta` reads `.claude.json`. After: discovery
  dispatches on the vendor to `ReadMeta` or to the Codex auth reader, and
  both fill a vendor-neutral identity on the account: email, vendor account
  id, and whether the document was readable. Claude Code's cached usage
  stays on its `Meta`.
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
  the account id the body names when it names one.
- **The request ledger and stored responses.** Owner: the state package.
  Protects: `Claim` is the single authorization for traffic. Today
  `state.Open(accountsRoot)` already scopes the store to one root, and the
  quiet period is a package-level constant read through `spacing()`
  (`internal/state/state.go`). After: each scope opens its own store under
  its own accounts root with its own spacing. `state.Key` and its `ID`
  encoding do not change. The store a body was read from says which vendor's
  parser reads it.
- **Refresh.** Owner: the refresh package. Protects: no retry inside a run;
  the permit, not an earlier flag, lets a request leave. Today a candidate
  holds a key, a dir and a token, and `Start` takes one URL
  (`internal/refresh/refresh.go`). After: a candidate is built by the
  vendor's access reader and binds the key, the prepared request (URL and
  headers), the vendor, and what the 401 re-read needs. Its fields stay
  unexported so the key, the account header and the token cannot be
  mismatched by a caller.
- **Facts.** Owner: accountstate. Protects: the three axes stay
  independent. After: an observation also carries the allowance, and the
  fresh window follows the scope's spacing rather than one global constant.
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

Vendor dispatch happens at three places and nowhere else: reading identity
at discovery, reading access (health and the candidate) at preparation, and
parsing a usage body. Everything else that differs between vendors is data
on the scope.

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
cfg, err := config.Load()
for _, scope := range cfg.Scopes()      // claude first; only present scopes
accts := accounts.Discover(scope)       // each Account carries scope
a, err := accounts.Select(scope, accts, selector)
prepared, err := launch.Prepare(a, accts, os.Environ())
reading, err := usage.Parse(scope.Vendor, body)
st := state.Open(scope.AccountsRoot, scope.Spacing)
```

**The limit row.** `usage.Row` keeps its fields and their Claude Code
meaning. It gains `WindowSeconds` (0 when the vendor does not state a
duration, which is every Claude Code row) and `Feature`. For a Codex row the
decoded identity is:

| field | Codex value |
| --- | --- |
| `Kind` | the slot: `primary` or `secondary` |
| `Group` | the limit object the window sits in: `rate_limit`, `code_review_rate_limit`, or `additional` |
| `Feature` | `metered_feature` of an `additional_rate_limits` entry; empty otherwise |
| `Model` | always empty |
| `WindowSeconds` | `limit_window_seconds` |

These are the vendor's own words. The label is derived from them alone: the
duration named (`weekly`, `5h`, otherwise days or hours), then the limit's
`limit_name` or "code review" where the group is not `rate_limit`. A compact
board column is identified by kind, group, model, feature and window
seconds together. Two accounts whose `primary` slots have different
durations therefore get different columns. A window whose kind or duration
cannot be decoded has a bad identity state and is not given a column, as
today. A limit object with `limit_reached: true` gives its rows a
non-normal severity.

**Reset meaning.** `ResetState` gains one value.

| state | entered when | rendered | written by |
| --- | --- | --- | --- |
| ok | a parseable future or past instant | countdown; past means rolled over | either parser |
| none | the vendor legitimately states no reset | a dash | either parser |
| unstarted | Codex: `used_percent` is 0 and `reset_after_seconds` ≥ `limit_window_seconds` | "not started"; `ResetAt` is 0 | the Codex parser |
| bad | present but unparseable | `reset?` and a drift marker | either parser |

An unstarted row never counts as rolled over.

**The allowance.** An observation carries one account-level allowance.

| state | entered when | effect |
| --- | --- | --- |
| unknown | the document does not say (every Claude Code response; a Codex response missing the fields) | none; never blocks |
| allowed | Codex `rate_limit.allowed` is true and `limit_reached` is false | none |
| blocked | Codex `allowed` is false or `limit_reached` is true | `Actionable` is false; caption says the limit is reached |
| bad | the fields are present under a wrong type | treated as unknown for actionability; drift tag; `check` fails |

Unknown is never read as allowed, and an empty window list is never read as
unlimited.

**Codex health.** There is no vendor probe on the board path; the auth
document decides.

| `auth.json` | health | attempt when health is ok |
| --- | --- | --- |
| absent | no login | — |
| unreadable, not JSON, or missing `tokens` under `auth_mode: "chatgpt"` | bad blob | — |
| an auth mode other than `chatgpt` | unknown, caption names the mode | — |
| ChatGPT login, access token `exp` in the future | ok | pending |
| ChatGPT login, access token expired or its `exp` undecodable | ok | token stale |
| ChatGPT login, `tokens.account_id` absent | ok | identity unknown |

"Relogin required" is never produced for Codex: the document carries no
refresh-token expiry, and the project reads expiry only on positive
evidence.

**Identity and the ledger key.** The Codex key's identity is
`tokens.account_id` joined with the `chatgpt_user_id` claim, so two homes on
one login share a budget and two people in one workspace do not. It goes in
the existing `Key.UUID` field. A live response whose `account_id` disagrees
with the account's is recorded as unparseable and never shown. The same
check runs on replay.

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

**Command surface.** `--vendor <claude|codex>` defaults to `claude` on
`launch`, `resolve`, `accounts add`, `accounts remove` and `limits`, so
every existing invocation means what it meant. On `accounts` and `--json`
the flag restricts output to one vendor; absent, both are shown. The
launcher format and primary-name pin for Codex are
`HEADROOM_CODEX_LAUNCHER_FORMAT` (default `headroom launch --vendor codex
--account %s`) and `HEADROOM_CODEX_PRIMARY_NAME`. `HEADROOM_CODEX_ACCOUNTS_ROOT`
(default `~/.codex-accounts`) and `HEADROOM_CODEX_USAGE_URL` re-point the
rest, under the same absoluteness rule. `HEADROOM_HOME` re-points both
vendors' trees.

**`--json` schema 5.** `accounts[]` stays one flat list. Each account gains
`"vendor"`. Top-level `current` becomes an object keyed by vendor, holding
only present vendors. Each limit gains `window_seconds` (omitted when 0) and
`feature` (omitted when empty); `reset_state` may be `"unstarted"`. `usage`
gains `allowance`: `"unknown" | "allowed" | "blocked" | "bad"`. `source`
keeps `"live"`, `"headroom_cache"` and `"claude_cache"` with their meanings; a
Codex account never reports `claude_cache`. `problems[]` entries gain `"vendor"`.
The session document is unchanged.

**Shared configuration for a Codex home.** `accounts add --vendor codex
--share-config` links a whitelist from `~/.codex`: `config.toml`,
`AGENTS.md`, `themes`, `skills`, `prompts`, `rules`, `plugins`. It is a
whitelist for the reason Claude Code's is: `auth.json`, `history.jsonl`, the
sqlite stores and `sessions/` must not be shared by accident. Nothing
load-bearing rides on the list.

### Wiring — the changed paths

Account preparation:

- Today: `app.prepareWith` reads the Keychain blob per account, asks
  `claude auth status` in parallel, resolves health from both, and calls
  `refresh.Prepare` when health is ok (`internal/app/app.go`).
- After: `prepareWith` dispatches once per scope to the vendor's access
  reader, which returns plan, health and either a candidate or the attempt
  state that blocks one. Claude Code's reader is today's body moved. The
  Codex reader is the health table above over one `auth.json` read.

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
  the visible list. `due()` and `startRound` run for the visible page only.
  The one-shot print and `--json` run one round per present scope and print
  the pages in vendor order under a heading line each.

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
Fallback: if the rule misfires the row shows a full-window countdown; the
states and the wiring do not change.

**P5. One shared session store.** Settled. Basis — measured in scratch
homes. With only `sessions/` symlinked to another home's, the second home
resumed the first's session by id, built its own sqlite index by backfill,
appended to the same rollout, and later found a session created after its
backfill completed. A session created under one account (plan `pro`,
containing an encrypted reasoning item) resumed correctly under the other
(plan `prolite`); that one rollout now records both plan types and no
account identity. This is why rollout usage snapshots are unattributable.
Does not establish: the desktop app's tolerance of rollouts it did not
index; `history.jsonl` under a shared store.

**P6. Ambient credentials override the home.** Settled. Basis — established
from the `openai/codex` source tree (login crate, `load_auth` in the auth
manager:
`CODEX_API_KEY` where the caller enables it, which `codex exec` and the main
CLI paths do; `CODEX_ACCESS_TOKEN` unconditionally) and measured: in an
empty home, `CODEX_ACCESS_TOKEN=<junk> codex login status` fails parsing the
junk as a token, so the variable is read. `OPENAI_API_KEY` is read only by
the realtime feature and is left alone.

**P7. Request spacing.** Assumed: 90 seconds, the existing value. No
rate-limit headers were returned and no 429 was provoked. Fallback: the
value is one field on the Codex scope; backoff on a 429 already exists in
the ledger. A budget far stricter than Claude Code's changes that number and
the board's cadence, not the shape.

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
   after the upgrade. Real: the store, discovery, the renderer.
2. Obligation: scope resolution. Observe: `config.Load` under the
   environment overrides; relative values refuse for both vendors; presence
   is false when neither Codex directory exists.
3. Obligation: the Codex auth reader implements the health table. Observe:
   the reader's public result over a table of documents, one per row of the
   table plus `account_id` disagreeing with its claim.
4. Obligation: the Codex usage parser implements the row, reset and
   allowance tables. Observe: parsed readings for the P3 body; a body with a
   secondary window of another duration; an additional limit; a code-review
   limit; the unstarted shape; `allowed: false`; a percent under a wrong
   type; a missing `rate_limit`. Two windows that differ only in duration
   have different column identities. Limit: shapes not observed live are
   built from the source structs.
5. Obligation: one parse dispatch. Observe: a stored Codex body replays
   through `limits` and passes `check`; a Codex body whose `account_id`
   names another account is not shown; a Claude Code body still parses.
6. Obligation: actionability honours the allowance. Observe: `Actionable`
   over facts with each allowance state; unknown never blocks.
7. Obligation: refresh for Codex. Observe: through `refresh.Start` against a
   local HTTP server: both headers present; 200 stores the body in the Codex
   store only; 429 defers with backoff; 401 leaves health and rows and
   annotates the attempt; no second request leaves in one run. Substitute:
   the HTTP server, which proves the request and interpretation and not the
   vendor's behaviour.
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
    `VerifyTopology` accepts, creates an absent canonical `sessions/` store,
    refuses a store that is a symlink or a file, and links only whitelisted
    config.
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
14. Obligation: `check` for Codex. Observe: over a fixture tree, PASS for a
    sound tree; FAIL on a drifted auth or usage document; INCONCLUSIVE on
    429, transport failure and a stale token; a home where `codex login
    status` says logged in and `auth.json` is absent is reported as an
    unsupported credential store; an isolation probe (`CODEX_HOME` = an
    empty temp dir must answer not logged in) fails loudly if the installed
    version stops honouring the variable; a corrupt Codex `state.json`
    reports as headroom's own file.
15. Obligation: the `--json` schema 5 contract and `--vendor` filtering.
    Observe: the serialized document for a two-vendor fixture.
16. Obligation: the real thing works. Observe: on the owner's machine,
    `headroom check`, the board, and one launch per Codex account. Limit:
    manual, not repeatable in CI, and it spends one request per account.

## Delivery

One PR, built as phases on the `codex-support` branch because each later
phase stands on the one before. Two sessions at most.

1. **The reshape.** Scopes in `config`, accounts carrying their scope, the
   store opened with its spacing. Claude Code only; obligation 1 green
   before anything else is written.
2. **Reading Codex.** The auth reader, the usage parser, the row, reset and
   allowance extensions, the parse dispatch, the candidate carrying its
   request, and the Codex checks that verify those documents. Obligations
   3–8 and 14.
3. **Showing Codex.** Pages, the tab bar, the one-shot print, schema 5,
   `limits --vendor`. Obligations 12, 13, 15.
4. **Acting on Codex.** The environment policy, `launch`, `resolve`,
   `accounts add`, the removal refusal. Obligations 9–11. Then
   `docs/REFERENCE.md`, `README.md`, `DESIGN.md` (a Codex "system observed"
   section from the premises above), `CLAUDE.md` and both shipped skills in
   the same PR, because command and schema wording is their contract. Then
   obligation 16.

Open decisions: none.
