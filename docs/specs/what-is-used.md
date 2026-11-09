# What is used — headroom keeps what its owner runs

Status: decided 2026-11-09 after a usage review and one consult round
(`consult-r1`, claude-fable-5-1); built on the `simplify/what-is-used`
branch, with the steward change on a planlab branch and the callers on a
dotfiles branch. Kept as the dated record of what was retired and why; the
design as it stands lives in `DESIGN.md`.

## Summary

headroom had grown to 22.7k non-test Go lines in 28 packages. Three surfaces
carried most of the growth, and none was in use:

- **Launch evaluation.** It needed every placement input logged and the rule
  named and frozen.
- **The session picker.** It needed two writes into vendor files and a
  re-home-first ordering in the store.
- **The second home.** It needed the store to hold two documents under
  ordered locks.

Their requirements shaped the code that stays. This change retires them,
moves the one consumer of the second home (planlab's steward) onto headless
tokens, and keeps every surface with a caller.

## Evidence

The usage review covered three sources:

- **headroom's launch logs** on mac, mini and mini2, from 2026-10-01 (mini2
  from 10-30);
- **the owner's agent sessions** on all three machines;
- **a grep of the calling repositories:** `~/dotfiles`, envoy, brief,
  gazette, skill-evals, drive, gwt, twin and planlab.

The findings:

- **The picker:** 1 launch among 2,079 interactive Claude launches, on
  2026-10-02. The owner: "I dont actually use headroom sessions, so long as
  x --resume works i am fine".
- **`launch --last`:** none.
- **`launches --eval`:** run only while it was being built, and once for the
  review itself. It merged on 10-10; the placement rule last changed on
  10-05.
- **The second home:** used only by steward on mini, which launched nothing
  from 11-01. All five of its own logins had expired, and every daily turn
  refused with "no account can take a launch: … (login expired)".
- **`resolve`, `accounts ledger` and `launch --auto`:** called only by
  steward.
- **`HEADROOM_ACCOUNTS_ROOT` and `HEADROOM_CODEX_ACCOUNTS_ROOT`:** set
  nowhere outside headroom's tests.
- **Headless:** 1,380 skill-evals placements on mini, 10-13 to 11-05.

## Decisions

Retired, with their code, tests, contract pins, help, docs and skills text:

- **`headroom launches`, `--eval`, `usage.jsonl`, and the packages `eval`,
  `usagelog` and `jsonl`.** The placement rule loses its name and its frozen
  lines. A decision stops carrying its inputs for a log, and `state.Place`
  stops returning the record it read.
- **`headroom sessions`, `--cd-file`, its `--json`, the `r` and `dd` writes,
  and the hidden `resume` tombstone.** Also the transcript tail reader, the
  vendor's store-name munge, HEAD and branch reading, and `launchop`'s
  picker request. Clikit's unknown-command error already exits 2 with
  nothing on stdout, which is all the tombstone guaranteed.
- **The second home:** `.ledger`, `accounts ledger`, member registration,
  the two-document store, other homes' registry scan, another home's share
  in load (`load.homes` in the JSON document, which stays schema 8 without
  it: no caller read the field), `check`'s audit
  of a shared ledger, and the spelled-out primary under a non-login `HOME`.
- **`resolve`, `launch --last` and `launch --auto`.**
- **`HEADROOM_ACCOUNTS_ROOT` and `HEADROOM_CODEX_ACCOUNTS_ROOT`.** Both roots
  derive from `HEADROOM_HOME`, so the check that two roots are not one
  location goes with them.
- **The terminal picker of a bare `accounts remove`.** A bare remove lists
  the candidates and refuses.

Changed:

- **The launch log.** `launches.jsonl` keeps one slim line per launch and
  headless placement: time, vendor, mode, reason, the account chosen,
  working directory, session and pid. It is appended with `O_APPEND`, never
  rewritten, read by no code, and unbounded. At mini's rate that is a few
  MB a year, and the review that produced this record needed forty days of
  it.
- **`HEADROOM_HOME`.** It is the documented way for a process in another
  `HOME` to address this home's headroom. Under it, a launch of the primary
  refuses; headless, the board and `limits` work.

Kept, each with a caller:

- the board, `--compact` and `--json`;
- `limits` and `refresh`;
- `launch`, with `--account`, `--vendor` and `--dry-run`;
- `check` and `version`;
- `accounts add`, `remove` and `reserve`;
- `login`: the batch, `start`, `finish`, `status`, `cancel`, and
  `--dry-run --json`;
- every `headless` command;
- Codex;
- the remaining knobs.

Callers, on the dotfiles branch: the `x-select` picker binding goes with
the picker, and `remote-logins` reads Codex account names from `limits
--vendor codex` rather than the board's `--json`. Both come from the same
discovery and spend no request, so the change is the caller's, not
headroom's.

## Steward on tokens

Steward runs Claude through envoy, whose `ENVOY_CLAUDE_CMD` names a
launcher prefix. The prefix becomes steward's own wrapper, which:

1. runs `headroom headless place` and prints the document's notes on stderr;
2. on exit 3 prints the refusal on its one `no session started:` line and
   exits 2, as on every exit before claude. envoy, seeing no result, records
   an infra failure with the prompt's fate unknown; steward reads the line
   as proof the prompt never ran, so the attempt retries after its backoff;
3. runs `headroom headless token <account>` and exports it as
   `CLAUDE_CODE_OAUTH_TOKEN`;
4. reports its own pid with `headroom headless started <placement> $$`;
5. prints its account line;
6. execs `claude "$@"` under steward's one config dir.

The owner's home, where the tokens are, is the wrapper's alone. The
installer writes it into the shim on the sessions' `PATH`, the wrapper sets
`HEADROOM_HOME` on its own headroom calls, and steward's prelude clears the
variable, so nothing a session runs addresses the owner's logins. The doctor
derives the home from the same `STEWARD_HOME` the installer wrote. It asks
`version` and `headless --json`, never `check`: under a re-pointed
`HEADROOM_HOME`, `check` fails its `home:` line by design, since the primary
it would describe is not the one the vendor resolves.

The tokens are the allow list. Minting one is the owner saying the account
may be spent headlessly, and a reservation still keeps an account out of
other repositories. Each turn is placed afresh: envoy names the session
itself, and the transcript lives in steward's own store.

Two spikes settled the premises, on 2026-11-09 against `main` at `ea7cf76`
and Claude Code 2.1.319:

- **The protocol (VERIFIED).** It ran against the real binary in a fixture
  home: a wrapper whose `HOME` is another home places, takes the token,
  reports `$$` and execs a stub vendor. While that runs, the owner's dry run
  shows the account `busy 1`, and a second turn placed meanwhile goes to the
  other account. After both exit, each counts as `pending 1`.
- **Token precedence (VERIFIED).** A config dir holding an expired stored
  login reports `authMethod: claude.ai`; with `CLAUDE_CODE_OAUTH_TOKEN` set
  it reports `authMethod: oauth_token`.

Not settled by a spike: whether WebSearch and WebFetch run on a setup token.
The owner accepted that risk.

## Tenets

- **One quota document per accounts root, under one lock.** A second
  consumer joins by reporting its process, never by sharing the file from
  another home.
- **A program with its own config dir spends a subscription through a token
  and a placement**, never through a login it does not hold. A second set of
  logins is a second renewal cycle, and the cycle is what failed.
- **Placement's record serves the next launch, not an audit.** Once the
  record must explain itself, every input climbs into it and the rule grows
  a name.
- **The launch log explains and no code reads it.**
- **The picker goes, owner following stays.** The re-home a launch writes
  for a named session lands `x --resume <id>` on the right account; its
  sweep runs inside the same lock.
- **Steward speaks the headless contract and nothing private.** A verb or
  flag for one caller is the second home again by another route.

## Test plan

What must still hold, each by a test:

- `x --resume <id>` from a checkout follows the session's owner, records a
  move when it places elsewhere, and the sweep keeps a young re-home.
- A reservation refuses automatic choices elsewhere and binds headless
  placements by the caller's directory.
- A headless run reported with `started` counts as busy on every surface
  until its process exits, then as a recent launch. That includes an exec'd
  process keeping the reporter's pid.
- Every launch and headless placement appends exactly one slim line, and an
  unwritable log is a note, never a refusal.
- `headroom sessions`, `resume`, `resolve`, `launches`, `accounts ledger`,
  `launch --last` and `launch --auto` exit 2 with nothing on stdout.
- A `state.json` carrying `members`, `legacy_import`, `placements.by_home`
  and a `home` on placements and runs, from an older binary, reads, places
  and writes without error. Retired sections are carried or dropped, never
  a refusal.
- In steward: a launch the wrapper refused retries after the backoff, an
  unreadable token starts nothing, an unreported process still runs, and a
  session's environment names no headroom home.
- `limits` and the board's `--json` carry schema 8 without `load.homes`.

A `.ledger` left in an accounts root has no test: no code reads the file,
so one would restate the deletion. The verification spike placed with one
present.

## Owed

In order: `twin sync headroom` moves the owner's headroom on mini before
steward's deploy pins the new build. An older owner binary labels steward's
runs by the home they no longer carry.

By hand, once every machine runs the new build:

- **Old evaluation log:** delete `usage.jsonl` under both accounts roots.
- **Steward's second-home leftovers on mini:** delete
  `~/.steward-home/.claude-accounts/.ledger` at steward's deploy, which ends
  the window where an old owner binary still scans steward's registries.
  Also delete steward's extra account dirs and the stale
  `~/.steward-home/.claude/.credentials.json`.
- **The stray `qiushi` token key** in mini's `headless.json`. It names no
  account since the primary became `.primary`.
