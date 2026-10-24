# Headless — a headless runner spends the subscriptions

Status: built on the `headless` branch, 2026-10-13; kept as the dated record
of the decision and as the contract a caller is built against — the commands,
exit statuses and JSON below. The design as it stands lives in `DESIGN.md`
§ Headless tokens. Written against `main` at 5f6b22c and Claude Code 2.1.296;
every measurement below was taken on the owner's laptop on that date. A real
mint is owed (§ Owed): minting needs a person to click Authorize.

## Summary

Current: skill-eval runs each test session as `claude -p` under a throwaway `HOME` and `CLAUDE_CONFIG_DIR`, inside `sandbox-exec`, billed to one subscription through a `claude setup-token` token in `CLAUDE_CODE_OAUTH_TOKEN`.
Current: `headroom launch` cannot serve it: there an account is its config dir, which carries the shared settings, skills, hooks and session store a test must keep out, and headroom must run outside the sandbox.

Goal: a caller that brings its own config dir asks headroom which subscription the next session spends and gets that account's token, so its sessions spread across the accounts the way automatic launches spread.
Goal: those sessions are load in headroom's own placement, so the owner's launches steer around eval work and `launches --eval` sees it.

Change: `headroom headless mint <account>` mints a long-lived token through `claude setup-token`, approved in the account's Chrome profile (or at the printed URL, with the code pasted back, over ssh), and stores it in a file only the user can read.
Change: `headroom headless place` runs the automatic launch's rule over the accounts holding a token, records the decision as load and prints it as JSON; it refuses, saying when room returns, when every account it could take is near a limit.
Change: `headroom headless started <placement> <pid>` reports the session's process, which then counts as busy on that account for as long as it runs.
Change: `headroom headless token <account>` prints that one account's token; nothing else ever prints one.

Boundary: the account's interactive login is untouched. A token is a separate grant that never refreshes the login, so the login is never lent out (`CLAUDE_SECURESTORAGE_CONFIG_DIR`, a copied credential file): every session would refresh it from a config dir that does not hold its lock.
Boundary: `headless` sets no environment and execs no session. `launch`'s invariant — the child environment is a function of the decision — is untouched because there is no child.
Boundary: Claude Code only, on this machine only: the token file lives in the Claude Code accounts root, and the mini's login Keychain is closed over ssh.
Boundary: headroom cannot tell which account a token belongs to. The account is the user's word: the profile it was approved in.

## Vendor facts

Measured on 2.1.296, laptop, 2026-10-13:

- `claude setup-token` runs the same OAuth flow as `claude auth login`, asking
  for scope `user:inference` only and a lifetime of 31536000 seconds. It
  takes no `--email`, so the sign-in page is not pre-filled.
- It opens the page by exec'ing `$BROWSER` with the authorize URL as its only
  argument (recorded with a stub), exactly as `auth login` does, so headroom
  as `$BROWSER` opens it in the matched Chrome profile.
- It accepts the code pasted at its own `Paste code here if prompted >`
  prompt (a bogus code reached the token endpoint and came back 400).
- It stores nothing: on success it prints the token and the line `Your OAuth
  token (valid for 1 year):` — the lifetime from the expiry the endpoint
  granted, rounded to days — and writes no credential. It runs under a
  scratch config dir, where it leaves only `.claude.json` and `backups/`.
- With its output on a pipe it draws whole frames of plain lines, one after
  another — no cursor movement and no wrapping. Its input must be a
  terminal: with input on a pipe it drew nothing and waited.
- At the paste prompt it ignores every key — ctrl-c, a fast double or
  triple ctrl-c, ctrl-d, Esc — on a terminal and with its output piped. It
  exits on SIGTERM and SIGHUP.
- `claude auth status` with only `CLAUDE_CODE_OAUTH_TOKEN` set answers
  `{"loggedIn": true, "authMethod": "oauth_token", …}` with no email or
  organization, from local state — a made-up token reads the same. It can
  say neither whose a token is nor whether it works.
- `sandbox-exec -f <profile> <cmd>` execs in place: the pid a caller's
  `Popen` returns is the session's own process.

Two facts wait on a real mint (§ Owed): whether the vendor exits by itself
after printing the token — headroom does not depend on it: once the token is
captured it gives the vendor five seconds and then ends it — and whether
spend through a token shows in the account's usage figures, as it should,
since a setup-token is an OAuth grant of the subscription's own.

## Commands

Every command is Claude Code only and takes no `--vendor`.

### `headroom headless mint <account>`

Needs a terminal. Refuses an account whose dir is not logged in, whose login
cannot be read, or (an extra) whose dir is logged in as another email: the
token is counted against the subscription the dir is logged in to, and that
login is how headroom reads its figures.

Runs `claude setup-token` with input on the terminal, output on a pipe, under
a scratch config dir made for the run and removed after, with the environment
`launch`'s constructor builds for that dir, minus inherited credentials. Its
`$BROWSER` is headroom (`HEADROOM_BROWSER_PROFILE` naming the profile matched
to the account's email, as for `login`); over ssh from another machine it is
headroom opening nothing, as for `login`, and the user opens the printed URL
and pastes the code. The output
reaches the terminal with every `sk-ant-…` string replaced by `[token withheld
— headroom stores it]`. The approval is waited for up to 10 minutes (the
vendor ignores ctrl-c, so closing the terminal is the way to stop sooner).

A token-shaped string printed by the vendor is stored: value, mint time, end
(mint time plus the lifetime the vendor stated; unknown if it stated none),
the account's ledger key and email at mint. A second mint replaces the first;
the first is not revoked.

### `headroom headless place [--prefer <account>] [--exclude <account>]… [--dry-run]`

Prints one JSON document on stdout. Exit 0: placed. Exit 3: refused — read
`refusal` and `retry_at`. Exit 1: headroom could not decide or record (stderr
says why). Exit 2: usage.

Candidates are the accounts of this home, built as a launch builds them —
figures, verified-live busy sessions (headless runs included) and launches of
the last fifteen minutes, from every home sharing the ledger. An account is
excluded, on positive evidence only, when it has no token, its token's end has
passed, its dir is now logged in to another subscription or to none, or its
identity could not be read just now — and when the caller names it with
`--exclude`, repeatable, read as `excluded by the caller`. The caller knows what headroom cannot: a session that ended on
the account's usage limit, or a token the vendor refused, whose account reads
idle to the rule. Excluding it inside the placement keeps the choice the
rule's and records nothing on an account the caller would veto. A name
nothing answers to exits 1; an account both preferred and excluded exits 2. The
rule is `placement.Choose`, the automatic launch's, with `CanWait`: when every
account left is near a limit (any figure at 80% or more) it refuses instead of
taking the least spent one. An account at its limit with extra usage on would
keep running and bill credits; at 80% it is already set aside. A refusal
records nothing and logs nothing; a placement is recorded like a launch — load
for fifteen minutes, under an id — logged with mode `headless`, and leaves the
usual detached refresh behind when an account could be asked.

`--prefer <account>` follows that account while it has room — for a session's
later turn, so its prompt cache stays warm — and otherwise moves, as a launch
follows a session's owner. `--dry-run` decides and records nothing.

```json
{
  "schema": 1,
  "placed": true,
  "account": "yan@planlab.ai",
  "placement": "4f1c9a2e7b3d5c60",
  "reason": "least-load",
  "rule": "load-2",
  "token_ends_at": "2027-10-13T09:12:44Z",
  "retry_at": null,
  "notes": ["its headless token ends Oct 18 (in 5d) — headroom headless mint yan@planlab.ai"],
  "accounts": [
    {
      "account": "yan@planlab.ai",
      "eligible": true,
      "near_limit": false,
      "load": 1, "busy": 0, "pending": 0,
      "observed_at": "2026-10-13T08:58:02Z",
      "stale": false,
      "session": {"percent": 12, "basis": "observed", "resets_at": "2026-10-13T12:00:00Z"},
      "week": {"percent": 33, "basis": "observed", "resets_at": "2026-10-16T12:00:00Z"}
    },
    {"account": "qiushi", "eligible": false, "excluded": "no headless token — headroom headless mint qiushi", "...": "…"}
  ]
}
```

Refused:

```json
{
  "schema": 1,
  "placed": false,
  "rule": "load-2",
  "refusal": "every account that could take it is near a limit: yan@planlab.ai (5h session 86%); not eligible: qiushi (no headless token — headroom headless mint qiushi)",
  "retry_at": "2026-10-13T12:00:00Z",
  "accounts": [{"account": "yan@planlab.ai", "eligible": true, "near_limit": true, "room_at": "2026-10-13T12:00:00Z", "...": "…"}]
}
```

`reason` is the rule's: `least-load`, `rotation` (nothing was ever
observed), `owner` (`--prefer` followed) or `moved` (`--prefer` near a
limit). A figure's `basis` is `observed`, `stale` (older than fifteen
minutes: a lower bound), `ended` (its window has reset: counted as 0) or
`bad`. `retry_at` is when the first account set aside near a limit has room
again: the latest reset among its rows at 80% or more; an excluded account's
resets never count. It is `null` when no such account names its resets — and
when every account is excluded, since then waiting alone changes nothing: an
account needs a token, or a person. With every account excluded the refusal
names each with its reason: `no account can take a launch: qiushi (no
headless token — headroom headless mint qiushi), yan@planlab.ai (excluded by
the caller), …`. A token's reason names the command that fixes it.
`notes` warns when the chosen token ends within seven days.

### `headroom headless started <placement> <pid>`

Reports the process that took the placement. headroom reads its start instant
(`ps`), gives the placement that pid — so the placement and the process count
once — and records a *run* in the subscription ledger: `{placement, account,
name, pid, started_ms, home}`. Every surface that counts load (a launch, the
board, `--json`, `headless place`) counts a run as a busy session of its
subscription while its pid runs with that start instant, exactly as a vendor
registry claim is verified. When the process exits, its placement counts out
the rest of its fifteen minutes, and the next write that sees it gone drops
the run; any run is dropped twelve hours after it started. Silent on success.
Exit 0 too when the process has already exited (its placement still counts);
exit 1 for an id no recent placement carries — a placement is forgotten after
fifteen minutes, so report right after starting the session.

### `headroom headless token <account>`

Prints that account's token and a newline, nothing else. Exit 1 with a reason
on stderr when it has none, when its end has passed, or when the dir is now
logged in to another subscription than the token was minted for — the
placement's reasons, on positive evidence only: a dir logged out, or whose
identity cannot be read just now, still holds the subscription its token was
minted for.

### `headroom headless [--json]`, `headroom headless forget <account>`

The listing names every account, whether it holds a token, when that token
ends and why a placement would not choose it; never a value. `forget` drops a
stored token (headroom does not revoke it with the vendor); `accounts remove`
forgets the removed account's.

## Long sessions

A placement counts as load for fifteen minutes, like a launch. A session can
run longer, so the caller reports its process and the process counts as busy
while it runs: the same fact headroom already counts — a verified-live
session — from a record the caller supplies instead of a vendor registry it
cannot read (the session's config dir is the caller's, and gone afterwards).
Nothing is asked of the caller when the session ends: liveness is read from
the process table. A caller that never reports gets fifteen minutes per
placement and nothing more.

## The token file

`~/.claude-accounts/headless.json`, mode 0600, created and replaced
atomically under a lock: `{"v": 1, "tokens": {"<account>": {"token",
"minted_at_ms", "ends_at_ms", "account", "email"}}}`. A file others can read
is refused, not repaired — the user must see that a secret may have been read
— and a file that does not decode is refused, never written over. The token is
in no other file: not `state.json`, not either log, not the placement
document.

## Learning that a token ends

`headroom login`'s plan, which names each login's end, adds a line for each
headless token that ends inside its window (7 days, or `--within`) or has
ended, with the `mint` command that renews it; the listing shows every end;
`place` notes it for a token within seven days of its end and excludes one
whose end has passed.

## For skill-eval

Per session, outside the sandbox:

```python
r = subprocess.run(["headroom", "headless", "place"] + (["--prefer", prev] if prev else []),
                   capture_output=True, text=True)
doc = json.loads(r.stdout)
if r.returncode == 3:      # refused
    if not any(a["eligible"] for a in doc["accounts"]):
        ...                # no token usable: waiting changes nothing — stop and say doc["refusal"]
    ...                    # else wait until doc["retry_at"], or poll every few minutes if it is null
elif r.returncode != 0:    # headroom failed: r.stderr
    ...
token = subprocess.run(["headroom", "headless", "token", doc["account"]],
                       capture_output=True, text=True, check=True).stdout.strip()
env["CLAUDE_CODE_OAUTH_TOKEN"] = token
p = subprocess.Popen(["sandbox-exec", "-f", profile, "claude", *args], env=env, ...)
subprocess.run(["headroom", "headless", "started", doc["placement"], str(p.pid)])
out, err = p.communicate(timeout=...)
```

- Place per `claude -p` process, including the `--resume` turn, passing
  `--prefer <the account the first turn ran on>`: the session's transcript is
  in the caller's config dir, so any account can continue it, and the
  preference only keeps its prompt cache warm.
- A session that ended on an API error — a usage limit, a revoked or invalid
  token — is retried with `--exclude <its account>`, and every account
  excluded earlier for that slot, instead of vetoing an answer and placing
  again.
- `run_claude` changes from `subprocess.run` to `Popen` so `started` can be
  sent between spawn and wait.
- Workers may place at once: placements serialize on the ledger's lock, and
  each counts the ones before it.
- Never print or log the token; pass it in the environment only. The one-
  subscription `SKILL_EVAL_OAUTH_TOKEN` path can stay as the fallback.
- A refusal is the normal "every account is busy for the owner" answer: hold
  new rollouts, not running ones.

## Owed

**Closing read owed** (no PR: the `headless` branch, merged locally) — ready
when: the owner has minted a token for one account with `headroom headless
mint` and run one session of a few minutes on it. Read: the mint ended on its
own with `✓ … headless token stored`, without the five-second stop; and the
account's 5h figure, read by `headroom refresh` a minute after the session,
moved on that account and on no other.
