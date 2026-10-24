# The command line on clikit

Status: shipped from the `clikit` branch, 2026-10-24; § Owed is what remains.
The record of what the migration was built from: what the grammar is, which
differences callers see, how the code is reshaped around it, and the cases
that hold it. Written against `main` at dd103a3, clikit v0.4.1 and Kong
v1.16.1, after one consult round (`consult-r1/codex-gpt-6-astra`) whose
findings are folded in below.

## Summary

Current: twelve hand-written argv parsers in `internal/app` and one hand-kept
usage string; `config.Load` runs before help, so `HEADROOM_HOME=relative
headroom --help` exits 2 with no help. The package also carries its seams as
mutable globals and writes to the process's own streams.

Goal: every command declared once, as a Kong struct in `cmd/headroom`, so help
is generated from what parses and works whatever the configuration says; the
command layer only translates; the work below it takes its streams and its
edges as values.

Change: `cmd/headroom` holds the grammar on clikit; `internal/app` keeps the
surfaces, one typed entry each; two protocols move into packages that own
their whole sequence — `internal/launchop` (gather, preview or commit a
placement) and `internal/round` (one vendor's round: rows, requests, results).

Boundary: every invocation in the callers' inventory prints the same bytes
and exits the same way, except the differences listed below, which the owner
approved. Docs (README, REFERENCE, DESIGN, CLAUDE.md, skills) are the owner's
separate pass; this file and the help pages are part of the work.

## Grammar

```
headroom [--compact|--json] [--vendor v]           board (default)
headroom accounts [--vendor v] [--compact|--json]   accounts board (default)
         accounts add <email> [--share-config[=<dir>]]
         accounts remove [<account>] [-y]
         accounts ledger [<root>]
headroom launch [--vendor v] [--auto|--last|--account n] [--remember|--dry-run] [args…]
headroom sessions [--json] [--cd-file abs] [args…]
headroom limits [--account n] [--vendor v]
headroom refresh [--vendor v]
headroom launches [-n N] [--json] [--eval] [--since S] [--vendor v]
headroom resolve [<name>] [--vendor v]
headroom login [<name>…] [--all] [--within days] [--dry-run]
headroom headless [--json]                          headless list (default)
         headless mint <account> | place [--prefer a] [--exclude a]… [--dry-run]
         headless started <placement> <pid> | token <account> | forget <account>
headroom check | version | --version
(hidden) resume …                                   tombstone, exit 2
```

- **Vendor.** The root board takes `--vendor` as a filter (absent: every
  vendor present). `accounts` takes it on the parent, so it reaches `add`,
  `remove` and `ledger` from any position, as `takeVendor` did; there it
  defaults to Claude Code for `add` and `remove`, filters for the board, and
  `ledger` refuses anything but Claude Code. `launch` and `resolve` default to
  Claude Code; `limits`, `launches`, `refresh` filter.
- **A default command's flags belong to it.** Kong parses a `withargs`
  default's flags at its parent's level, so `accounts --compact remove x` and
  `--json sessions` would reach the other command with the flag silently
  dropped. A root hook refuses them before anything runs.
- **Passthrough** is `passthrough:"partial"`: flags are headroom's until the
  first value; Kong keeps a leading `--` in the slice and the command strips
  exactly one. Vendor arguments no longer need `--` (approved); an unknown
  flag before them is still refused.
- **The tombstone** is a hidden command with command-level passthrough, so
  `resume --help` and `resume --version` reach it too.
- **Account names.** A selector flag or argument refuses `""` (an expansion
  bug, never "the pinned account") except `accounts remove ""`, which keeps
  meaning the bare form. Repeated `--exclude` takes each value whole
  (`sep:"none"`).
- **`--share-config`** takes a value only as `--share-config=<dir>`, so a bare
  flag before the email does not swallow it.
- **`-n`** is `--count` with a short form; whether it was given is read from
  the parse, so `--eval -n 20` is still refused. `--within` refuses negative
  and overflowing day counts.
- **Configuration** loads when a command asks for it. `check` loads it itself,
  so a refused configuration still prints its `FAIL  config:` line.
- **The `$BROWSER` opener** (`headroom https://…` with the opener variables
  set) is dispatched in `main` before the grammar: the vendor runs it, and no
  grammar holds a URL as a command.
- **Exit codes**: 0; 1 failure; 2 invalid argv, `check` INCONCLUSIVE, the
  tombstone; 3 `headless place` refused.

## What callers see differently (approved)

1. Help pages are clikit's, and work under any configuration.
2. Argv errors read `headroom: error: <msg>` with the synopsis and a `--help`
   pointer; failures read `headroom: error: <msg>`. The launch announcement
   (`headroom launch: <account> · <how> …`) and the picker's
   (`headroom sessions: …`) are output, not errors, and keep their prefix.
3. A configuration headroom refuses exits 1 (was 2).
4. Vendor arguments are accepted without `--`.
5. `select` and `--check` are retired.
6. A mistyped command reports `unexpected argument <word>` under the board's
   synopsis: Kong hands unknown words to the default command. A clikit release
   that restates it is a separate change (see Owed).
7. Kong's wording for flag conflicts, the reason moving to the command's help;
   and for a negative number given to a flag as its own word: `--within -1`
   and `-n -1` read as a short flag (`expected int value but got "-1" (short
   flag); perhaps try --within="-1"?`). Still exit 2; `--within=-1` gets
   headroom's own message.
8. New spellings: `headroom board`, `headless list`, `accounts --json`,
   `headroom --compact`, `--count`.

## Shape

`cmd/headroom` translates: Kong structs, `Help()` and sections, and `Run`
methods that build a typed request, make one call and map the outcome to a
status. It holds the live edges (`host`), bound once; a test appends its own.

`internal/launchop` owns a placement from request to record. A caller hands
it a request — the set, the store, how the account is decided, the session it
is for, whether to remember — and gets back a plan it can preview (the dry
run's table, through the same refusals) or commit (`.current`, the store's
locked place, the log line, the refresh left behind). Gathering, candidate
eligibility, owner lookup for an automatic launch naming a session, and the
announcement's words are its own. The board asks it for marks — load and the
next account — keyed by account name, and applies them to its own rows. A
headless placement and `started` are the same protocol with a caller-supplied
eligibility rule and no exec.

`internal/round` owns one vendor's round: discovery, the facts on disk,
access (health and the request candidate — the second place vendor documents
are read), the claim and fetches, and applying results. Results carry their
round (`Fetch` reports through `Feed`, which stamps each one), and `Apply`
ignores another round's. Callers read presentation facts and drive the
lifecycle; `FromDisk` is the limits read, which carries no request, and `Of`
is a round over facts a caller already holds, which asks nobody. When the
board's next round is due is a pure function of the snapshot, the accounts,
whether any can be asked, the spacing and the time (`nextRound`).

`internal/app` keeps the surfaces — the board picker and print, the session
picker, login, headless, the account set, the launch and session logs,
limits and the JSON documents — each one typed entry taking its writers and
the edges it uses. No package-level seams, no `os.Stdout`. The `$BROWSER`
opener (`approval.go`) stays here: both of its users, login and mint, are
surfaces here, and `main` reaches it through `app.Open`.

Dependencies are per module, as function fields: launchop takes the
credential reader, the process probe and the detached refresh; round the
credential reader and the health probe; a surface takes what it calls,
including the two terminal facts (stdin, stdout), which surfaces need
differently — the board, the picker and remove need both, mint only stdin.

## Tests

- `cmd/headroom/contract_test.go`: the built binary over a fixture home with
  stub vendors and no terminal — 64 caller shapes and refusals, recorded from
  the binary before the migration (`HEADROOM_CONTRACT_UPDATE=1`). A pin
  changes only for an approved difference, in the commit that causes it; the
  grammar switch changed 18, each one of the differences above, every exit
  status kept but the refused configuration's.
- `cmd/headroom/grammar_test.go`, in process through `clikittest.Run` with a
  test `host`: `TestGrammarRefuses` (every argv refusal exits 2 with nothing
  started and the home unchanged), `TestGrammarDecodes` (what reaches the
  vendor, names with commas, `--vendor` positions, `-n` presence),
  `TestBrokenConfiguration`, `TestCommandsOnAnAbsentVendor` (moved from
  `internal/app`) and `TestHelpPages` (`clikittest.Golden`, `make golden`).
- Surfaces are tested through their typed entries; launchop and round through
  their interfaces; `go test` never reaches live state (each package's
  `TestMain` clears the inherited environment). App tests that used to spell
  argv refusals point to `TestGrammarRefuses`.
- Final verification runs with `GOWORK=off` against the tagged clikit.

Planned cases, each a trap the consult named:

- `accounts --vendor codex add x`, `accounts add --vendor codex x` and
  `accounts add x --vendor codex` all seed a Codex account; `accounts
  --vendor codex remove x` refuses as Codex removal does.
- `accounts --compact remove --yes x` and `--json sessions` exit 2 having
  changed nothing.
- `resume`, `resume --help`, `resume --version`, `resume --json x`, and
  `resume` under a refused configuration: the tombstone on stderr, nothing on
  stdout, exit 2.
- `headless place --exclude a,b` excludes one account named `a,b`;
  `--exclude ""` is refused.
- `accounts remove ""` behaves as `accounts remove`.
- `launches --eval -n 20`, `--eval -n 0` and `--since 7d` without `--eval`
  are refused; `login --within -1` and an overflowing count are refused.
- `launch -- -- --fast` hands the vendor `-- --fast`; `launch --` hands it
  nothing; `launch --resume x` is refused; `launch fix it` hands it `fix it`.
- `HEADROOM_HOME=relative headroom --help` prints help, exit 0; `check` under
  it prints the `FAIL  config:` line, exit 1.
- The board prints one frame when stdin or stdout is not a terminal; mint
  needs only stdin to be one (`TestEachSurfaceReadsTheTerminalFactsItNeeds`).
- A round's result applied to another round is ignored
  (`TestApplyIgnoresAnotherRoundsResult`, red without the check).

## Tenets

- **Invocation syntax has one declarative owner.** Translation ends at typed
  requests, so help and execution cannot grow separate grammars.
- **Operations own ordering and matching state.** A caller cannot pair one
  account set with another snapshot, or skip a refusal a commit would make.
- **Compatibility is demonstrated against the old executable.** Output, child
  argv, routing and effects stay fixed except for the recorded differences.
- **Dependencies follow the operation that needs them.** Help and disk-only
  reads never initialize credentials, processes or terminals.
- **The released dependency is the tested dependency.** A local workspace
  cannot stand in for what a clean checkout builds.

Unless you know better ones.

## Owed

- A clikit release restating the unknown-command error at a `withargs`
  default, and refusing a default's flags under another command in the engine
  rather than in each tool; then a `go.mod` bump and the root hook deleted.
