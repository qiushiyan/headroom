# Antigravity support — a third vendor, on the board only

Status: built on the `agy-support` branch, 2026-11-24. Written against `main`
at fc4b068, agy 1.5.13 (Homebrew cask `antigravity-cli`), macOS.

## Summary

Current: headroom shows quota for Claude Code and Codex accounts and launches
either vendor on the account the rule chooses.
Current: the owner reaches Gemini models through Google's Antigravity CLI,
`agy`, on one Google AI Ultra account per machine, and reads its quota only
inside `agy` (`/usage`).

Goal: `headroom accounts` gains an Antigravity page beside Claude Code and
Codex, with the same three-axis honesty: figures with their age, a failed read
said apart from the figures it leaves standing.
Goal: `limits`, `accounts --compact`, `--json`, `refresh` and `check` cover
Antigravity wherever it is installed, through the round, the claim ledger and
the one-parser rule every vendor goes through.

Change: the vendor set grows to `claude`, `codex`, `agy`. Every vendor branch
that read "Codex, else Claude Code" becomes an explicit choice per vendor,
held by a test that no two-way form is left.
Change: a request is either an HTTP GET (Claude Code, Codex) or one run of the
vendor's own binary (Antigravity), bound into the candidate at preparation.
Change: a usage reading can name the email it was measured for, and an
Antigravity row is labelled by its newest reading's email.
Change: an attempt can end `vendor_failed`, the vendor's binary exiting
non-zero.

Boundary: Antigravity is observed, never launched. `launch`, `login` and
`headless` do not take `--vendor agy`; `accounts add`, `remove` and `reserve`
refuse it; no placement marks its page.
Boundary: headroom never reads, copies or presents Antigravity's login. It
runs `agy` and reads what `agy` prints.
Boundary: one Antigravity account per machine — the macOS user's sign-in.
Unverified: what `agy -p /usage` prints while signed out (not measured: it
would mean signing the owner out). It lands as `vendor_failed` with its exit
code, whatever it is.
Open: no decision waits on the owner.

## Intent

The owner will run one Antigravity account for the foreseeable future and
wants its quota beside the others. Load balancing is not wanted for one
account, and § Antigravity observed shows it is not possible either: `agy`
keeps one sign-in per macOS user.

### Non-goals

- **Launch, placement, login, headless tokens, account management.** With one
  sign-in per user there is no second account to place on or switch to,
  `agy`'s own browser sign-in is the login, and handing its token to another
  program is what Google suspends Antigravity accounts for
  (`~/dotfiles/docs/antigravity-cli.md` § Install and sign-in). Each command
  that owns one of these refuses Antigravity in plain sight.
- **G1 credits.** `/credits` reports a prepaid balance that only matters when
  `useG1Credits` is on; the owner's balance is 0 and the setting is off.

## Antigravity observed

Measured on agy 1.5.13, 2026-11-24.

- **The quota read is a print-mode slash command.** `agy -p /usage
  --output-format json` answers in about two seconds with
  `{"status":"SUCCESS","num_turns":0,"usage":{…0 tokens…},"command":{"name":"usage","data":{"groups":[…]}}}`.
  It runs no model turn and writes no conversation and no history entry. The
  changelog keeps piped and `--json` output in UTC "so existing scripts are
  unaffected", so Google treats it as a scripting surface. `/quota` is an
  alias that answers the same document.
- **The document.** `command.data.groups[]` each have a `name` ("Gemini
  Models", "Claude and GPT models") and `buckets[]`: `id` (`gemini-weekly`,
  `gemini-5h`, `3p-weekly`, `3p-5h`), `name`, `window` (`weekly`, `5h`),
  `remaining_fraction` (0–1) and `reset_time` (RFC 3339 UTC). Models within a
  group share its two limits. A window nobody has spent against reports
  `remaining_fraction` 1 and `reset_time` `1970-01-01T00:00:00Z`.
- **An API key has no account quota.** Under `GEMINI_API_KEY` the command
  exits 2 (changelog), so the read strips it.
- **One sign-in per macOS user.** The login is one Keychain item with a fixed
  name (service `gemini`, account `antigravity`). Moving the state dir
  (`ANTIGRAVITY_GEMINI_DIR`) left the sign-in where it was: the run
  authenticated from the same item. Nothing documented holds a second login.
- **Identity is in the run's log, nowhere else.** No settings, cache or
  Keychain attribute names the Google account, and no print-mode command does
  (`/config` does not). Each run logs `applyAuthResult: email=<address>,
  authMethod=consumer, …` once it authenticates, and `--log-file <path>` puts
  that run's log where the caller chooses — leaving `agy`'s own log dir and
  its `cli.log` link untouched.
- **What a read leaves behind.** Nothing in its working directory; with
  `--log-file`, nothing in `~/.gemini/antigravity-cli/log`. `agy` refreshes
  its conversation-summary index on start, as any run does.

## Design

### The vendor

`config.Agy` (`"agy"`, titled "Antigravity") is third in `config.Vendors`. Its
scope:

- primary dir `~/.gemini/antigravity-cli`; presence is that dir existing, so
  a machine without the cask shows no page and `check` notes it;
- accounts root `~/.agy-accounts`, holding `state.json` and nothing else —
  discovery yields the primary alone, whatever the root contains;
- binary `agy`, resolved on `PATH`;
- spacing five minutes: each read starts a whole `agy` process that
  authenticates and asks Google several questions, and the windows are five
  hours and a week long;
- launcher `agy`, display only.

The account's name is `primary` (`PrimaryName` with no email at discovery),
so its ledger key is `dir:primary` and never moves.

### Identity travels with the reading

Discovery reads nothing of Antigravity's — there is nothing on disk to read.
The fetch builds headroom's own envelope as the stored body:

```json
{"email": "<from the run's log>", "agy": <agy's stdout, verbatim>}
```

`usage.Parse(agy, …)` reads the email and the windows from that one document,
live, replayed and in `check` alike. `Reading.Email` carries it; the
observation keeps it; the row's label and `--json`'s `email` are the newest
observation's email. Before the first read the row is labelled `primary`. A
log without the line leaves the email empty: the figures still show, because
this machine has one sign-in for them to belong to, and `check` fails the
identity line as drift.

### The request

`refresh.Candidate` binds one of two transports at preparation: the GET it
always was, or a run of the vendor's binary — path, arguments, working
directory and environment. `PrepareAgy` binds
`agy -p /usage --output-format json --log-file <temp>` from the accounts root,
with the environment minus every variable that substitutes a credential or
re-points the state (`config.Agy.EnvPolicy().Strip`, names read from the
1.5.13 binary; only `GEMINI_API_KEY`'s effect is documented). The claim,
completion, spacing and stored body are the ledger's, unchanged.

A run's outcome:

| outcome | attempt | ledger |
|---|---|---|
| exit 0, `status` SUCCESS, a usage document | `ok` (or `no_limits`) | stored |
| exit 0, anything else | `unparseable` | spent |
| exit non-zero | `vendor_failed`, exit code kept | failed |
| could not start, or past 10 s | `transport_error` | failed |

`vendor_failed` is a new attempt state: the vendor's own program declined,
which is neither the network nor an endpoint's status. Exit 2 is captioned as
the API-key case the vendor documents; any other code names itself and sends
the person to `agy`, never to a login. The ten-second bound is the HTTP
client's, so `refresh` still ends within about twelve seconds. The run's log
goes to a temporary file that is removed once read.

### Health

headroom has no way to ask `agy` whether it is signed in short of the read
itself, and reads nothing of the login. An installed Antigravity (the scope is
present) is taken as usable: health `ok` on the live surfaces, `unprobed` from
`limits`, as for every vendor. A read that fails says so on the attempt axis
with the exit code and points at `agy` itself, and never claims the account is
logged out; a request's outcome never writes health
(`TestApplyWritesEachAxisOnItsOwn`).

### Commands

What a vendor supports is decided by the command that owns it:

- `limits`, `refresh`, `--json` and the board take `--vendor agy`
  (`vendorFilter`); `accounts --vendor agy` shows its page.
- `launch` takes `--vendor claude|codex` only (`vendorOne`), so
  `launch --vendor agy` is a usage error before configuration is read.
- `accounts add`, `remove` and `reserve` with `--vendor agy` refuse: "Antigravity
  keeps one sign-in per macOS user; headroom adds, removes and reserves no
  accounts for it".
- The board's marks (`launchop.Mark`) are not gathered for Antigravity, its
  compact page carries no legend for the mark, and its footer never says
  that a relocated home refuses a launch (`PrimaryLaunchRefused`).
- `launch.Extra` refuses Antigravity and `accounts.Seed` refuses it
  (`accounts.ErrOneSignIn`), below the commands that already do.
- `check` runs an Antigravity group after Codex's: the binary on `PATH`, one
  read through the round's own candidate (the document parses, its fields are
  well-formed, the log names an email), and headroom's own state file.

### Contracts

`limits` and `accounts --compact` gain Antigravity rows on machines that have
it. The schema stays 8: no field changes; `vendor` and `attempt.state` each
gain a value. Every consumer filters by vendor — the quota mod's lane picker
(`vendor == 'claude'`), `claude-quota-refresh.sh` (`--vendor claude`),
`remote-logins` (`--vendor codex`) — and the quota mod's `/quota`, which prints
`accounts --compact` verbatim, shows the row as it shows Codex's.

## Verification

1. `usage.Parse(agy, …)` maps every bucket to a row by its `id`, with
   `window_seconds`, the spent percent, the unstarted epoch and the envelope's
   email; a malformed field is tagged, never zero; a document from another
   command, or one that failed, is unparseable.
2. The log reader takes the email from the run's own `applyAuthResult` line and
   from nothing else.
3. A run of a stub `agy` lands as each row of § The request's table, through
   the claim and completion, and the stored body replays to the same figures.
4. The run's environment carries none of the stripped variables, and its
   working directory is headroom's.
5. The board shows an Antigravity page with one row and no marks; `limits` and
   `accounts --compact` carry it (contract fixtures, with a stub `agy` that
   records the run's arguments and working directory).
6. `launch --vendor agy` exits 2 with nothing on stdout; `accounts add`,
   `remove`, `reserve` with `--vendor agy` refuse.
7. No vendor branch reads "Codex, else Claude Code" (`TestNoVendorBranchAssumesTwo`).
8. `check` runs the Antigravity group against a stub, and notes an absent one.
9. Live, on this laptop: `headroom accounts --vendor agy`, `headroom limits
   --vendor agy` and `headroom check` against the installed `agy`.
