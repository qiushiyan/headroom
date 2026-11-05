# Claude Code's entrypoint label, and the marks a session leaves on its children

Assessed 2026-11-05 on Claude Code 2.1.318 (macOS, this laptop and the mini),
headroom at `ea7cf76`. Prompted by
[anthropics/claude-code#97074](https://github.com/anthropics/claude-code/issues/97074),
which reports that `claude -p` drains the 5-hour window about twice as fast as
the interactive mode, and by magpie's move off `claude -p` in response. The
question was whether headroom should change how it launches Claude Code. The
spike harness beside this file reproduces every measurement below.

## Decisions

- **headroom does not relabel a launch.** It never sets
  `CLAUDE_CODE_ENTRYPOINT`. The workaround circulating on #97074 (a tool names
  itself, and its print-mode work is metered like the interactive mode) still
  works on the wire. But the gain on these accounts is small (below), and the
  label exists to say which client sent a request: replacing the vendor's own
  label to escape a rate attached to it is the kind of thing the consumer
  terms forbid, and Anthropic has said nothing about whether it is allowed.
  2.1.318's new request-name variable rejects a name that starts with `sdk`
  or is already an entrypoint name, so Anthropic treats the label as
  meaningful. Magpie, the nearest peer, declined the same workaround.
- **headroom does not drive the interactive mode in place of `-p`.** A launch
  replaces itself with the vendor and never proxies. A stream-json shim over a
  pseudo-terminal, which is what magpie built, would belong to the caller that
  wants stream-json (envoy). Magpie's own issue (below) shows what keeping that
  shim correct costs.
- **A launch removes the two session markers that change the child.**
  `config.EnvPolicy`'s `Markers` (for Claude Code, `CLAUDE_CODE_CHILD_SESSION`
  and `CLAUDE_CODE_ENTRYPOINT`) are removed from every child headroom starts,
  without a notice, since every shell inside a session carries them. The
  vendor therefore labels each launch by how it actually runs, and a caller's
  own label does not survive `headroom launch`. Pinned by
  `TestEnvStripsSessionMarkers` and `TestALaunchInsideASessionStartsItsOwn`.

## When to revisit

- **Anthropic documents the labels, or says a tool's own label is acceptable:**
  headroom could then set one on the launches that run in print mode.
- **A 5-hour window returns on most accounts, or print mode grows well past
  7% of the weighted volume:** measure the meter first, on the account that
  still has the window (Not measured, below).
- **A Claude Code release changes what a marker does:** rerun `run.sh`. A
  newly added variable that hides a session or changes its label joins
  `Markers`.

## What prompted it

#97074 and its thread hold eight or more independent A/B tests (2.1.282
through 2.1.317, Linux, macOS, Windows, Pro through Max 20x). For the same
tokens, print mode, labelled `sdk-cli`, fills the 5-hour window about 1.7 to
2.5 times faster than the interactive mode's `cli`. `claude-desktop` and
`claude-vscode` are metered like `cli`. On the weekly window, one controlled
test measured 1.14 to 1.32 times. A pooled test and a cache-heavy workload
showed no measurable difference. The workaround, `CLAUDE_CODE_ENTRYPOINT=<own
name>` on the print-mode process, has shipped in t3code, Paseo and xMatrix.
Anthropic had not answered as of this date.

Magpie (yetone/magpie) is a gateway that runs Claude subscriptions. In
`43966a53f` it began running Claude Code's interactive mode in a pseudo-terminal
in place of `claude -p`, leaving the label unset because the interactive mode
"is what its terms describe". In `e5f66f8a6` it stopped falling back to `-p`,
because "a -p request counts as another client's". Its issue #4804 is that
design's cost: a client in auto mode changed the request's safeguards on every
tool batch, and each change restarted the interactive run and rewrote the whole
conversation into the prompt cache, 3 to 10 times the cache spend. It was fixed
in v0.1.2882. Its `fbbdf6e05` records Anthropic answering some interactive
turns that carry a third-party system prompt with a 400 that moves them to
extra usage. The server classifies clients by more than the label.

## Findings

### The label follows how Claude Code runs, unless a variable overrides it

A session sends its label in two places: the User-Agent (`claude-cli/<version>
(external, <label>)`) and the first system block (`x-anthropic-billing-header:
cc_version=…; cc_entrypoint=<label>;`). It also records the label in its
transcript and in `sessions/<pid>.json`. All four agreed in every arm.

| Inherited `CLAUDE_CODE_ENTRYPOINT` | Print mode sends | Interactive sends |
|---|---|---|
| none | `sdk-cli` | `cli` |
| `cli` | `sdk-cli` | `cli` |
| `sdk-cli` | `sdk-cli` | `sdk-cli` |
| `sdk-ts` | `sdk-ts` | `sdk-ts` |
| `headroom` | `headroom` | `headroom` |

Print mode overrides an inherited `cli` and nothing else. The interactive mode
takes whatever it inherits. 2.1.318's binary also reads
`CLAUDE_CODE_ENTRYPOINT_ON_REQUESTS`, which is not in the changelog. Its debug
line says it is read only when the entrypoint is `claude-desktop-3p`, and it
rejects names that start with `sdk` and reserved entrypoint names. It is
Claude Desktop's, and it changes nothing for a launch.

### What the label costs on these accounts

`headroom limits` showed weekly windows only on six of the seven Claude
accounts. Only `qiushi.yan@itell.ai` still had a 5-hour window, the window that
#97074's 2× is measured on. Over the 30 days to this date (`survey.py`,
tokens weighted by list price), print mode was a small share of the work:

- **laptop:** 35 of 110 sessions, 4% of the weighted tokens, 34 of them
  started by `headroom launch`;
- **mini:** 444 of 861 sessions, 7% of the weighted tokens, 246 of them
  matched to a launch line. Most of it is agent work that envoy (consult,
  review, steward's turns) and the other callers CLAUDE.md lists start through
  `headroom launch --`.

At the weekly ratios measured on #97074, relabeling would save about 0.5 to 2%
of a week's allowance.

### What a marker inherited from another session does

A running session sets these variables for every process it spawns. They are
what its Bash tool sees and its own environment lacks: `CLAUDECODE`,
`CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_CODE_SESSION_ID`,
`CLAUDE_CODE_SESSION_ATTENDED`, `CLAUDE_PID`, `CLAUDE_CODE_EXECPATH`,
`CLAUDE_CODE_MESSAGING_SOCKET` and `_TOKEN`, `CLAUDE_EFFORT`, and
`GIT_EDITOR=true`. The `env` block of `settings.json` shows up there too, but
it is the user's, and every launched session reads it again. Each marker was
set alone on a clean session in each mode:

| Inherited | Print mode | Interactive |
|---|---|---|
| `CLAUDE_CODE_CHILD_SESSION=1` | unchanged | no transcript, no `sessions/<pid>.json`; the screen says "Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION marker" |
| `CLAUDE_CODE_ENTRYPOINT` | as the label table | as the label table |
| any other marker above | unchanged | unchanged |

"Unchanged" covers the label, the transcript, the registry entry, the effort
requested (`CLAUDE_EFFORT=xhigh` still requested the default) and the messaging
socket, which the child never connected to.

The marker that hides a session hides it from headroom three times over. The
transcript is what the session picker lists and resumes, and
`sessions/<pid>.json` is liveness: placement's busy count, and the gate on the
picker's `r` and `dd` and on `accounts remove`. A launch inherits the markers
from any shell that descends from a session: a tmux server started inside one
(the same path as DESIGN.md's wrong-account incident), or an agent's own
shell. No live interactive session on either machine carried one on this date
(`live-markers.sh`), so the strip prevents a failure that has not yet happened.

## Not measured

- **The meter itself.** No quota was spent. A 5-hour A/B on the one account
  with that window would cost about 15 to 25% of one window. Telling 1.14×
  from 1× on the weekly window would take dozens of points on a Max plan.
- **Codex.** Whether Codex marks the processes it spawns was not examined, so
  its row has no markers.
- **Other effects of the markers,** beyond the label, transcript, registry,
  effort and socket: hooks, telemetry, permission prompts.

## Reproducing

```sh
# both tables, against a local stand-in for the API; nothing leaves the machine
docs/research/2026-11-05-claude-entrypoint-and-session-markers/run.sh [path-to-claude-binary]
# the 30-day share by label, read-only; on another machine: ssh <host> python3 -I - < survey.py
python3 -I docs/research/2026-11-05-claude-entrypoint-and-session-markers/survey.py
# which running sessions inherited a marker
docs/research/2026-11-05-claude-entrypoint-and-session-markers/live-markers.sh
```

`run.sh` gives each arm a fresh `HOME` and config dir, a fake OAuth token, the
API on 127.0.0.1 (`capture.py`) and a dead proxy for every other request. It
drives interactive arms through a pseudo-terminal (`drive_tui.py`) and prints
`summary.py`'s line per arm.
