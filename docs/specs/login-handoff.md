# Login handoff — an agent renews a login in commands that each return

Status: built on the `login-handoff` branch, 2026-10-27, after one consult
round (`consult-r1`, codex gpt-6-astra), the spikes and two review rounds
recorded below; kept as the dated record of the decision and as the
contract an agent that renews this machine's logins builds against — the
commands, the document, its states and reasons, and the exit statuses. The
design as it stands lives in `DESIGN.md` § Renewing logins. Written against `main` at 741038f and Claude Code 2.1.311,
macOS; measurements were taken on the owner's laptop on that date.

## Summary

Current: `headroom login` is one foreground conversation held by one terminal: the vendor prints the sign-in URL there, headroom reads the pasted code there, and the verdict prints there.
Current: an agent with shell access (Grokbot) renews the owner's logins by keeping that conversation alive in tmux, with three scripts that scrape the URL and type the code.
Current: whether headroom opens the approval page on this machine's screen is inferred from `SSH_CONNECTION`, so an agent whose tmux was started by an ssh from the machine to itself gets a page opened on a screen nobody watches.

Goal: an agent renews one account's login in two commands that each return — one that gives it the URL, one that takes the code and says whether the login took — with machine-readable results and nothing opened here.
Goal: an account that cannot be renewed from the agent's session is refused before the agent spends an approval on it.
Goal: an interrupted call is recoverable: no retry burns an approval, sends a code twice, or leaves the agent unable to learn what happened.

Change: `headroom login start <account>` starts the account's sign-in under a detached holder and prints its URL as JSON; `finish <account>` hands the holder the code from stdin and prints the verdict; `status` and `cancel` inspect and end it.
Change: `headroom login --dry-run --json` prints the plan as a document.
Change: the shared login code holds an account back on what the Keychain actually gave this session, refuses a renewal it reads only from the file while the Keychain holds the login, and says when Chrome's profile list could not be read rather than that no profile matched.

Boundary: headroom still never writes a credential and still drives no browser. The vendor's `claude auth login` performs the login; the approver signs in to claude.ai and clicks Authorize wherever it is.
Boundary: Claude Code only. `headless mint` stays terminal-only: `setup-token` needs a terminal on its input (measured 2.1.296).
Boundary: headroom cannot make a Keychain this session cannot read readable. It refuses the accounts that live there, early and by name; unlocking stays a person's act.
Boundary: no notion of which inbox an account's mail reaches. The account's email is the key an agent maps from.

## Vendor facts

Measured on 2.1.311, laptop, 2026-10-27, under a scratch `CLAUDE_CONFIG_DIR`
with a stub `$BROWSER`, input on a pipe and output on a pipe or file:

- `claude auth login --email <e>` prints plain lines on a pipe, no escape
  sequences: `Opening browser to sign in…`, `If the browser didn't open,
  visit: <URL>`, then `Paste code here if prompted > ` with no newline, and
  waits.
- The printed URL's `redirect_uri` is
  `https://platform.claude.com/oauth/code/callback`, whose page shows the
  `code#state` to paste. The URL handed to `$BROWSER` redirects to
  `http://localhost:<port>/callback`, which a browser on another machine
  cannot reach. Both carry the same `state` and the same PKCE challenge.
- It calls `$BROWSER` about 200ms after printing the URL.
- It reads the code from a pipe. A well-formed code the server rejects ends
  the sign-in at once: `Login failed: That code wasn't accepted. It may be
  incomplete, expired, already used, or from an earlier sign-in attempt. Sign
  in again to get a new code.`, exit 1. A malformed one (`bogus#`) gets
  `Invalid code. Please make sure the full code was copied.` and the prompt
  stays open.
- Started by a setsid'd parent with no terminal, it behaves the same.
- When its parent is killed it keeps running: its input reaching end of file
  does not end it (still alive 10s after the holder's `kill -9`).
- It writes only `.claude.json` and `backups/` into its config dir before a
  code is exchanged.

Measured on this macOS (Darwin 27): a unix socket path binds at 103 bytes
and fails at 104.

Not measured, owed (§ Owed): a successful login with output on a pipe, and
whether the vendor exits by itself after one; how the agent's own session
reaches the mini; the sealed-Keychain error string on this macOS.

## Commands

Every command is Claude Code only and takes no `--vendor`. Each prints one
JSON document on stdout for every outcome that concerns the account; stderr
carries only what is not an outcome (usage, an account nothing answers to,
headroom itself unable to run).

### The document

```json
{
  "schema": 1,
  "account": "a@x.com",
  "email": "a@x.com",
  "state": "awaiting_code",
  "reason": null,
  "problem": null,
  "url": "https://claude.com/cai/oauth/authorize?code=true&…&state=…",
  "started_at": "2026-10-27T14:44:02Z",
  "expires_at": "2026-10-27T14:54:02Z",
  "finished_at": null,
  "login_ends_at": null,
  "reused": false
}
```

`state` is the account's sign-in as it stands after the command:

- `none` — no sign-in was ever started for this account here.
- `awaiting_code` — the holder is waiting for the code from `url`'s page,
  until `expires_at`.
- `exchanging` — a code was sent to the vendor and its answer is pending.
  Never sent twice.
- `renewed` — the login took: `login_ends_at` is the new end.
- `failed` — the sign-in is over without a renewal; `reason` says why.
- `unknown` — the sign-in is over and headroom cannot tell whether the login
  took (its holder ended mid-exchange); the plan or the board shows the
  account's login as it now stands.

`reason` is a stable token, `problem` the same in words for a person, naming
what fixes it. `keychain_unknown` is also `finish`'s, state `awaiting_code`,
nothing sent, when `security` stops answering since `start`. `url` is present only while `awaiting_code`. `reused` appears
on `start` only. A finished sign-in's document stays readable through
`status` until the next `start` of that account.

Reasons:

| reason | state | means |
|---|---|---|
| `keychain_unreadable` | as it stood | the account's login is in the Keychain, which this session cannot read |
| `keychain_unknown` | as it stood | headroom could not tell whether the account's login is in the Keychain |
| `identity_unreadable` | as it stood | the account is logged in but nothing says as whom, so a new login could not be checked |
| `busy` | as it stood | another `headroom login` is renewing this account right now |
| `exchanging` | `exchanging` | this account's sign-in has a code being exchanged |
| `no_url` | `failed` | the vendor printed no sign-in URL headroom could read |
| `screen_changed` | `failed` | the printed URL is not the sign-in the vendor opened |
| `holder_failed` | `failed` | the holder could not start (stated in `problem`) |
| `code_malformed` | `awaiting_code` | the line is not shaped `code#state`; nothing was sent |
| `code_other_signin` | `awaiting_code` | the code's state is another sign-in's; nothing was sent |
| `keychain_closed` | `awaiting_code` | the Keychain stopped serving this account's login since `start`; nothing was sent |
| `record_failed` | `awaiting_code` | the sign-in's record could not be written, and a code goes only once the record shows it; nothing was sent |
| `vendor_rejected` | `failed` | the vendor refused the code; the login is unchanged |
| `vendor_ended` | `failed` | the vendor ended before any code was sent |
| `not_renewed` | `failed` | the vendor finished, and the stored login did not change |
| `wrong_account` | `failed` | the dir is now logged in as another email: the page was approved as someone else |
| `stored_elsewhere` | `failed` | the new login went to the file while the Keychain holds the one sessions use |
| `expired` | `failed` | no code came before `expires_at` |
| `cancelled` | `failed` | `cancel` ended it, or the holder was stopped |
| `holder_gone` | `failed` | the holder ended before any code was sent |
| `outcome_unknown` | `unknown` | the holder ended while exchanging |
| `no_reply` | as it stood | `finish` lost the holder's answer after sending: ask `status`; from `status` or `cancel`, the holder took the call and did not answer in time — it is busy, not gone |
| `protocol` | as it stood | the holder waiting is from another headroom build; nothing was sent |
| `no_attempt` | `none` | there is no sign-in to finish or cancel |

### `headroom login start <account>`

Holds the account back, before any approval, on the plan's evidence (exit 3:
`keychain_unreadable`, `keychain_unknown`, `identity_unreadable`, `busy`). A
sign-in already `awaiting_code` is returned as it is, `reused: true`, its URL
unchanged; one `exchanging` is refused (exit 3). Otherwise it starts the
holder, waits up to 30 seconds for the URL, and prints the document: exit 0
`awaiting_code`; exit 1 `failed` (`no_url`, `screen_changed`,
`holder_failed`). Nothing opens on this machine. It returns while the holder
waits; no stream of the caller is left open, so `ssh host headroom login
start x` returns.

### `headroom login finish <account>`

Reads one line, the `code#state` the page showed, from stdin, and hands it to
the holder. The holder sends it to the vendor only when it is shaped
`code#state`, carries the state of the URL `start` printed, and the account's
login can still be read where it was read at `start`; otherwise exit 3, state
`awaiting_code`, nothing sent — fix what `reason` names and finish again. A
sent code is never sent again: the holder marks the sign-in `exchanging`
before writing it, waits up to 60 seconds for the vendor, and reads the
login back as `headroom login` does. Exit 0 `renewed`. Exit 3 whenever this
call sent nothing and the sign-in is not over: a refused code, and a finish
that finds a code already `exchanging` — ask `status`. Exit 1 for every other
end; `outcome_unknown` calls for the plan before a new `start`. `finish` on a
sign-in already over prints its document (exit 0 if it renewed, 1 otherwise)
and sends nothing. A lost answer prints `no_reply`: the code may have been
sent — ask `status`, never `start`.

An empty stdin is a usage error (exit 2).

### `headroom login status <account>`

Prints the document. Exit 0 whenever it names the account, `none` included;
exit 1 when the record cannot be read. A holder that does not answer in
time leaves the record as written, `no_reply`.

### `headroom login cancel <account>`

Ends a sign-in `awaiting_code` (state `failed`, reason `cancelled`); exit 0.
Exit 0 too when there is nothing to end. Refuses one `exchanging` (exit 3).
Then `start` begins afresh.

### `headroom login --dry-run --json`

The plan, renewing nothing; `--json` without `--dry-run` is a usage error
naming `start` and `finish`. Exit 0.

```json
{
  "schema": 1,
  "within_days": 7,
  "accounts": [
    {
      "account": "a@x.com",
      "email": "a@x.com",
      "renew": true,
      "why": "login ends Oct 30 (in 3d)",
      "held": null,
      "held_reason": null,
      "login_ends_at": "2026-10-30T09:12:44Z",
      "headless_token_ends_at": null,
      "signin": "none"
    }
  ],
  "headless_tokens_error": null
}
```

`renew` is what a bare `headroom login` would renew with the same `--within`,
names and `--all`. `signin` is `null` when the sign-in's record cannot be
read. `held_reason` takes the start refusals' tokens. An end
that is absent or unreadable is `null`, never guessed. `signin` is the
account's sign-in state, as `status` would print it.

### The batch

`headroom login [<account>…]` is unchanged for a person at a terminal. Its
explicit name is `headroom login renew [<account>…]`, for an account named
like a subcommand.

## How it works

- **One sign-in per account dir, one lock.** `<accounts root>/.login/<key>.lock`
  (key: the first 8 hex digits of the dir's sha256) is held by whatever runs
  the vendor's login for that dir — a holder for its whole life, the batch
  while its login runs. `start` and `cancel` are serialized by a second lock
  beside it, so two `start`s give one holder and one URL.
- **The holder** is the hidden `headroom login hold <account>`, started by
  `start` in a session of its own with stdin on `/dev/null`, stderr to
  `<key>.log`, and the login lock handed over as an inherited descriptor,
  which it keeps from the vendor (a vendor that inherited it held the lock
  past a holder killed outright, and with it every later `start` off the
  orphan — found by test plan 13 through the binary); its stdout carries one
  document back to `start` and is closed. It is the
  vendor's parent and runs `claude auth login --email <e>` under
  `launch.Prepare`'s environment with headroom as a `$BROWSER` that records
  the sign-in's state and opens nothing.
- **The URL** is read off the vendor's output by one parser: an https URL
  carrying a `state` and a `redirect_uri` that is not localhost. When the
  opener records a state within 3 seconds and it differs, the sign-in fails
  `screen_changed`; no URL in 30 seconds fails `no_url`. headroom never
  builds a URL of its own.
- **The record** `<key>.json` is the document plus what only the holder
  needs: the holder's pid and start instant and the sign-in's state. The
  holder writes it whole on each transition, and a transition is taken only
  once it is on disk: a code goes out only after `exchanging` is written,
  so a dead holder's record never says no code was sent when one was. An
  unreadable record is an error, never a sign-in that was not started.
- **The vendor is registered from its launch** in `<key>.vendor` (pid and
  `ps` start), by whoever runs it — a holder, before any URL, or the batch —
  and forgotten once reaped. A vendor runs only while registered: one whose
  registration cannot be written is ended at once and the login refused
  (`holder_failed` for `start`). The holder keeps the inherited lock referenced
  for its whole life: an `*os.File` nothing refers to is closed by the
  collector, and the lock with it (review r1).
- **The socket** `<key>.sock` carries one JSON request and one reply per
  connection, each with `"v": 1`; a version the holder does not speak is
  refused before anything is sent. A path over 103 bytes fails `start`
  (`holder_failed`).
- **Bounds.** `awaiting_code` lasts 10 minutes from `start`; once a code is
  sent, the exchange has 60 seconds, then SIGTERM, then a kill 3 seconds
  later. A vendor that exits with an error failed, whatever changed
  meanwhile, since the change may be another login's; one headroom had to
  end after the bound is judged by its read-back alone. Every Keychain read
  is bounded to 10 seconds.
- **An owner that dies leaves its vendor running** (measured). The next
  `start`, `cancel` or batch login of the account holds the lock, finds the
  vendor registered, and ends it when that pid still runs with its recorded
  start — after the exchange bound when the record says a code may be in
  flight — then settles a record left live. A `start` whose holder dies
  before it is ready does so itself, still holding the lock. `status` and
  `finish` report such a sign-in from its record: `holder_gone` before a
  code was sent, `outcome_unknown` after. A holder that takes a call and
  does not answer in time is slow, not gone: `status` prints the record as
  written, `no_reply`.

## Shared changes

- **Held on what the Keychain gave.** An account is held when its Keychain
  item exists and its login did not come back from the Keychain (sealed over
  ssh, or locked), or when `security` could not say whether the item exists
  (an exit other than found or not found, or no answer in 10 seconds). The
  sealed-Keychain explanation stays for the ssh case.
- **Read-back by source.** After a login, an account whose Keychain item
  exists must read its new login from the Keychain; a renewal readable only
  from the file is `stored_elsewhere`. The plan, the code check and the
  read-back share the one rule. The read-back's words name no command;
  each surface adds its own retry.
- **Removal asks the same way.** `accounts remove` deletes the Keychain item
  only after `security` says it exists, and an item it cannot answer for
  stops the removal with the dir left in place; it used to read any failure
  as no item.
- **One vendor command.** The batch and the holder build `claude auth login`
  through one constructor; only what they attach to it differs.
- **The sealed probe** is asked only to word the plan's explanation, and is
  bounded like every Keychain question.
- **Chrome.** The plan says `no Chrome profile list at <path>` when there is
  none, `Chrome's profile list at <path> could not be read: …` when it cannot
  be read or parsed, and `no Chrome profile matches <email>` only when it was
  read.
- **`.login` is never an account**: discovery skips dot names in `.order`
  as it does in the directory.

## Test plan

Each case runs hermetically: a temp home, stub `claude` and `security`.

1. A login renewed into the file while the account's Keychain item exists is
   ✗ `stored_elsewhere` in the batch (red on `main`, where it reads ✓).
2. An account whose Keychain item exists and whose login did not come back
   from the Keychain is held, with no sealed Keychain; one whose item
   `security` cannot answer for is held `keychain_unknown`.
3. Chrome: no `Local State`, an unreadable one, an unparseable one and a
   readable one with no match give four distinct plan phrases.
4. `--dry-run --json` prints the document with `null` for unknown ends;
   `--json` alone is a usage error.
5. Through the real binary and a stub vendor that prints 2.1.311's screen:
   `start` returns while its holder lives, with every stream it was given at
   end of file; `finish` with the right code prints `renewed` and the new end.
6. A second `start` returns the same URL, `reused: true`, with one holder;
   two concurrent `start`s give one holder and the same URL.
7. A malformed code and another sign-in's code exit 3, still
   `awaiting_code`, and the vendor received nothing; the right code then
   renews.
8. `finish` after `renewed` prints the receipt and the vendor received one
   code in all.
9. `finish` whose reply is lost: `status` then prints the outcome.
10. No code before the deadline: `failed`, `expired`, and the vendor ended.
11. A code sent just before the deadline completes: the exchange bound
    governs once a code is sent.
12. A vendor that stores the login and hangs is ended after the exchange
    bound, and the sign-in is `renewed`.
13. A holder killed while `awaiting_code`: `finish` prints `holder_gone`, and
    the next `start` ends the orphaned vendor and starts afresh.
14. The batch holds back an account whose sign-in a holder holds, and `start`
    refuses `busy` while the batch renews it.
15. A request of another protocol version is refused and its code not sent.
16. Grammar: `login a@x.com` and `login renew start` are the batch; `login
    start a@x.com` starts.
17. A `.login` line in `.order` makes no account.
18. Through the binary, a holder whose collector runs keeps its lock: a
    second `start` reuses the sign-in (review r1).
19. A record that cannot be written keeps the code from the vendor: exit 3,
    `record_failed`, nothing sent (review r1).
20. A vendor started before its URL, whose holder is killed, is ended by the
    start that waited on it; a batch killed mid-login leaves a vendor the
    next `start` ends (review r1).
21. A holder busy past `status`'s wait is reported as recorded, `no_reply`,
    never `holder_gone` (review r1).
22. The document's `email` is whom the sign-in is for, while the dir is
    logged in as someone else (review r1).
23. While a code is being exchanged, `finish` exits 3 `exchanging` and sends
    nothing, `cancel` and `start` refuse it, and the exchange keeps its
    verdict (review r1).
24. A dead holder's vendor mid-exchange is waited for, not ended (review r1).
25. The batch holds the login lock while its vendor runs: a `start` of the
    account then is refused `busy` (review r1).
26. `security`'s answers: found, not found (exit 44), another failure, no
    answer in time, no `security` at all (review r1).
27. A vendor whose registration cannot be written is ended and `start`
    fails `holder_failed` (review r2).
28. `vendor_ended` for a vendor that ends before any code; a `wrong_account`
    verdict names `headroom login start`, never the batch; `keychain_unknown`
    at `finish` sends nothing (review r2).

## Verification

Spikes before the build (throwaway code against Claude Code 2.1.311 under a
scratch config dir, laptop, 2026-10-27):

| Claim | Ran | Verdict |
|---|---|---|
| A `start` returns through a caller that reads its streams to their end (sshd) while a setsid'd holder keeps the vendor waiting | 3 runs: the caller saw end of file in 72–114ms; the holder survived a SIGHUP to the caller's process group | VERIFIED |
| The vendor under a holder with no terminal prints its URL and waits | same 3 runs | VERIFIED |
| `$BROWSER` is called just after the URL is printed | one run: the record appeared within 200ms | VERIFIED; a 3s wait covers it |
| An orphaned vendor ends when its input reaches end of file | holder `kill -9`'d; vendor still alive 10s later | NOT VERIFIED — the design gained the recorded vendor identity and its cleanup |
| A malformed code ends the sign-in | `bogus#` (empty state): `Invalid code…`, prompt still open after 60s | NOT VERIFIED — only a well-formed code the server rejects ends it |
| `readBack` passes a login stored in the file beside a Keychain item | throwaway test on `main` | VERIFIED as a defect (`✓ a@x.com: logged in`); test plan 1 now pins ✗ |

Verification after the build, the built binary against the real vendor in a
scratch home, every code bogus:

- `start` printed `awaiting_code` with the vendor's own code-page URL
  (`platform.claude.com` callback, `login_hint` filled) in 0.55s; the
  calling shell got its streams back; the holder leads its own session.
- A malformed code and another sign-in's exited 3, `awaiting_code`, with
  the vendor still waiting; a second `start` returned it, `reused: true`.
- A well-formed bogus code reached the token endpoint: exit 1,
  `vendor_rejected`, the vendor's own sentence in `problem`; holder and
  vendor gone, `status` keeping the outcome.
- `cancel` exited 0 and ended the vendor; `finish` after it exited 1,
  `cancelled`.
- Holder `kill -9`'d: the vendor kept waiting; `finish` exited 1,
  `holder_gone`; the next `start` ended the orphan and began afresh.
- The real home's plan, read-only, is line for line the installed build's
  for all six accounts — none newly held — and created no `.login`.
- Chrome, reproduced: in a sandboxed agent shell `Local State` reads
  `operation not permitted`; the installed build printed `no Chrome profile
  matches <email>` for every account, this build names the refusal.

Review r2 found that a vendor whose registration failed still ran, and the
skill's recovery rules missing `exchanging` and `outcome_unknown`.

Review r1 (codex astra, warm from the consult, and claude opus, cold) found,
each pinned red before its fix: the holder's lock closed by the collector; a
code sent when its `exchanging` could not be recorded; vendors untracked
before their URL and in the batch; a busy holder reported gone; the
document's email taken from the dir; `finish` exiting 1 while a code was in
flight; the `wrong_account` words sending a handoff caller to the batch; and
four behaviours no test pinned.

Found through the binary, not the in-process tests: the vendor inherited the
holder's lock descriptor, so a killed holder's orphan kept the account
`busy` for good. The holder now keeps the lock from the vendor.

Not driven: a real renewal; a sealed-Keychain session (no key for an ssh to
this machine); the agent's own harness.

## Owed

- A real renewal through `start` and `finish`: it needs an approval.
  Whether the vendor exits by itself after a successful code with its output
  on a pipe is the open fact; headroom does not depend on it.
- How the agent reaches the mini, and whether the mini's logins live in the
  Keychain or in files: with an ssh-born session and Keychain logins, every
  `start` there is refused `keychain_unreadable` until someone unlocks the
  Keychain in that session.
- The sealed-Keychain string on this macOS: the held rule no longer depends
  on it; only the ssh explanation does.
