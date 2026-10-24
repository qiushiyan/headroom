package main

import "github.com/qiushiyan/clikit"

// The prose that is not one command's own: the root page, and the sections
// several pages return from one value.

const rootHelp = `Bare, headroom is the board (see headroom board --help). Two things decide where a bare
headroom launch goes: the board's enter pins an account, and its a key turns automatic
placement on, so each bare launch takes the least-loaded account instead.

An account is a config dir: Claude Code's primary is ~/.claude and the others are
~/.claude-accounts/<email>; Codex's are ~/.codex and ~/.codex-accounts/<email>. Commands
that report — the board, limits, launches, refresh — cover every vendor on this machine
unless --vendor names one. Commands that act on one account — launch, resolve, accounts
add and remove — act on Claude Code unless --vendor says codex. login, sessions and
headless are Claude Code only.`

var configuration = clikit.Section{
	Title: "Configuration",
	Body: `Nothing needs configuring on a standard install. These variables re-point the defaults:

  HEADROOM_HOME                    everything home-derived, for both vendors (test isolation)
  HEADROOM_ACCOUNTS_ROOT           where Claude Code's extra account dirs live
  HEADROOM_PRIMARY_NAME            the name ~/.claude answers to (default: its email's local part)
  HEADROOM_LAUNCHER_FORMAT         how the board spells an account's launch command (%s = name)
  HEADROOM_CODEX_ACCOUNTS_ROOT     where Codex's extra homes live
  HEADROOM_CODEX_PRIMARY_NAME      the name ~/.codex answers to
  HEADROOM_CODEX_LAUNCHER_FORMAT   the board's Codex launch command
  HEADROOM_USAGE_URL, HEADROOM_CODEX_USAGE_URL   the usage endpoints

A relative HEADROOM_HOME or accounts root, or a ~/.claude-accounts/.ledger naming no
usable accounts root, makes every command refuse until it is fixed; headroom check says
which.`,
}

const boardHelp = `The account board: a block per account with its usage limits — the five-hour session
window and the weekly ones, each with its percent used and when it resets — the account's
health, and the load an automatic launch counts on it. Open on a terminal, it refreshes
itself as often as the usage endpoint allows and goes quiet after five minutes without a
key; r asks now. Enter pins the selected account for bare launches and a turns automatic
placement on; either prints what it recorded and exits. Tab switches vendor, esc cancels.

Off a terminal — a pipe, a script, a hook — it prints the board once and exits 0. --json
prints it as a document after one refresh round, for a script that wants fresh figures;
headroom limits prints the same document from disk alone, without probing or asking.`

var boardDocument = clikit.Section{
	Title: "The JSON document (schema 7)",
	Body: `{"schema": 7, "generated_at": RFC3339,
 "current": {"claude": "<account>", ...},   the account a bare launch of that vendor takes
 "mode": {"claude": "pinned"|"auto"|"", ...},   "" when .current cannot be resolved
 "accounts": [{
   "vendor": "claude"|"codex", "name", "email", "launcher", "plan", "current": bool,
   "health": "ok"|"no_login"|"relogin_required"|"bad_blob"|"unknown"|"unprobed",
   "usage": null | {"observed_at": RFC3339, "source": "live"|"headroom_cache"|"claude_cache",
     "fresh": bool, "allowance": "unknown"|"allowed"|"blocked"|"bad",
     "limits": [{"kind", "group", "model", "label", "percent", "percent_state",
                 "resets_at": RFC3339|null, "reset_state", "identity_state", ...}]},
   "attempt": {"state", "http_status", "next_eligible_at"},   the newest request, not the account
   "load": null | {"value", "busy", "launched", "homes": [...]}}],
 "problems": [{"vendor", "section", "detail"}]}

Select limits by kind, group and model, never by label. A *_state of "bad" means the
field did not parse: its value is not a reading. usage.observed_at says how old the
figures are; load is null from limits, which reads no process table.`,
}

const launchHelp = `Execs claude — or codex under --vendor codex — on the account decided, with the child's
environment built from that decision alone: an inherited CLAUDE_CONFIG_DIR (for Codex,
CODEX_HOME and its credential variables) is stripped, never obeyed. The process is
replaced, so the vendor's exit status is the command's.

How the account is decided:
  bare          what the board recorded: the pinned account, or under auto the least-loaded
  --account N   that account, whatever the mode
  --auto        this one launch placed automatically; the pin is untouched
  --last        the account of the newest recorded launch

Every launch says on stderr where it went and why before the vendor starts, as one line:
  headroom launch: a@x.com · auto · 5h 12% · week 33%, resets in 2.1d · load 2 (next: b@x.com)
A figure that is not a fresh measurement says so: ≥12% is a lower bound from figures older
than fifteen minutes, "window ended" a window whose reset has passed, ?% a percent that
did not parse. Under auto, -- --resume <id> and -- --session-id <id> follow the account
that last drove that session while it has room, and record where it went.

Codex's sessions are shared across its accounts: headroom launch --vendor codex --account
<other> -- resume continues one on another account.`

var launchExamples = clikit.Section{
	Title: "Examples",
	Body: `  headroom launch                                   the recorded choice
  headroom launch --account a@x.com -- --resume 1f2e…   continue a session on a@x.com
  headroom launch --auto --remember                 turn automatic placement on, and launch
  headroom launch --dry-run                         what a bare launch would choose, and why
  headroom launch --vendor codex --account a@x.com -- login`,
}

const sessionsHelp = `An interactive picker over every Claude Code session on this machine — transcripts are
machine-global — that enters the chosen session's project dir and continues it, by exec, on
the account that last drove it. x continues it on the current account instead and records
the move (under auto, on the least-loaded of the others); r renames, dd deletes; both are
refused while the session is open. It needs a terminal on stdin and stdout: claude takes
them over.

--json lists the sessions instead, with no terminal: each one's id, title, project dir,
branch, owner and whether it is live now, beside "current" and "mode" as in the board's
document. --cd-file is for a shell wrapper: it is created empty first and holds the entered
dir only once a session was committed, so the shell can cd there after claude exits.

Arguments go to claude before the picker's own --resume; --resume and --continue are
refused, since the picker chooses the session.`

const limitsHelp = `What is already known about every account's limits, as the board's JSON document (see
headroom board --help), read from disk alone: no health probe, no credential read, no
request — about 10ms against the board's 300ms, so a statusline can ask often. health reads
"unprobed" and load is null. Fresher figures come from the board, headroom --json and
headroom refresh, all under the same per-account request budget.`

const launchesHelp = `The newest launches from the launch log, one line each: when, which account, how it was
decided, the load and weekly figure it was decided on, how long that week had left, and the
account that would have been next. --json prints the records as logged, one per line.

--eval judges automatic placement instead, per rule version: windows its launches went into
that reached a limit while another account had room, weekly room that may have renewed
unspent, and the launches this binary's rule would decide differently. These are observed
outcomes, not what another choice would have caused. It reads the launch logs of every home
on the ledger and the usage log, and asks nothing. --since narrows it to a span back from
now (7d, 36h, 90m) or from a date (2026-10-01).`

const loginHelp = `Renews Claude Code logins: one claude auth login per account, in turn, its approval page
opened in the Chrome profile matched to the account's email, or the default browser when
none matches. Bare, it renews each login that ends within --within days, has ended, or was
never made; names renew exactly those, and --all every account, so their logins end
together.

It prints the plan first — every account's login end and where each chosen one's page
opens (all --dry-run prints) — then reads each credential back after its login: ✓ with the
new end, or ✗ when nothing changed or the dir is now logged in as another email. It ends on
the earliest end among the logins renewed, and on a headroom login command naming the ones
that did not take, if any; then it exits 1.

Over ssh from another machine nothing opens here: open the printed URL where you are, in a
browser signed in as the account, and paste the code it shows. An ssh session cannot read
the login Keychain until security unlock-keychain runs in it; accounts whose login lives
there are left alone and named.`

const headlessHelp = `For a caller that runs Claude Code sessions headless under a config dir of its own — an
eval runner — and wants them spread over the subscriptions the way automatic launches
spread. headroom decides and records; the caller starts the session itself, on the
account's headless token. The caller's contract is docs/specs/headless.md.

Bare, it lists every account's headless token: whether there is one, when it ends, and why
a placement would not choose the account. Tokens are never shown here.`

const placeHelp = `Decides which account the caller's next headless session spends, records the decision as
load the person's own launches steer around, and prints it as JSON on stdout:
  {"schema": 1, "placed": bool, "account", "placement", "reason", "rule", "token_ends_at",
   "refusal", "retry_at", "notes": [...], "accounts": [{"account", "eligible", "excluded",
   "near_limit", "room_at", "load", "busy", "pending", "observed_at", "stale",
   "session", "week"}]}
It exits 0 placed, and 3 refused — every account it could take is near a limit or
excluded — with the document saying why and, in retry_at, when room returns (null when
waiting alone will not help). Report the session's process with headroom headless started
<placement> <pid>, and fetch the token with headroom headless token <account>.`

const accountsHelp = `Bare, the account board (see headroom board --help). Its subcommands change the set of
accounts: add seeds a new account's dir, remove deletes one, ledger shares the request
budget with a second home on this machine.`

const addHelp = `Seeds the dir for a new subscription: ~/.claude-accounts/<email> (or ~/.codex-accounts/
<email> under --vendor codex), its session store linked to the machine-global one so every
account sees every session. --share-config links a whitelist of the primary's config —
settings, skills, commands — into it; --share-config=<dir> links every entry of that dir.
A login, its history and its session registry are never linked, whatever the source holds.

Then log in once: for Claude Code, headroom launch --account <email> and /login in the
session; for Codex, headroom launch --vendor codex --account <email> -- login.`

const removeHelp = `Removes an extra account: deletes its Keychain item and its dir, and scrubs .order.
Refused while a session is live on it, or when that cannot be verified. Bare on a terminal
it offers a picker of the removable accounts; it asks y/N unless --yes. Transcripts are
machine-global and survive. Also removes stranded <name>.lock debris. Codex accounts are
never removed — headroom cannot tell whether a codex session is running on a home — and the
refusal names the dir to delete by hand.`

const ledgerHelp = `Claude Code only. With a root, this home spends against that accounts root's ledger from
now on — for a second home on this machine holding its own logins of the same
subscriptions: both then ask each subscription once per request spacing and count each
other's sessions and launches. Naming this home's own root stops sharing. Either way it
registers this home in the ledger and lists the homes registered there.`

const checkHelp = `Verifies the reverse-engineered vendor facts headroom relies on — credential, identity and
usage-response shapes, the binary's seams, the session store's topology and retention —
and headroom's own state, for every vendor on this machine. Run it after a Claude Code or
Codex update, or when the board looks wrong. One line per assertion: ok, FAIL, or ?? for
one it could not test (rate limited, offline, a stale token). It exits 0 when everything
passed, 1 on any FAIL — a configuration headroom refuses among them — and 2 when nothing
failed but something could not be tested.`

const refreshHelp = `Asks the usage endpoint about every account the budget lets it ask, stores the answers and
prints nothing: the round headroom --json runs, without the health probes or the document.
An automatic launch leaves one running for the next launch's figures. It ends within about
twelve seconds; SIGTERM, SIGINT or SIGHUP stop it within one, with the request it abandoned
recorded.`

const resolveHelp = `Prints one line for shell preflight: the account's canonical name, its config dir and its
kind (primary or extra), separated by tabs. Bare, the pinned account; under automatic
placement there is no one account to print, so a name is required.`
