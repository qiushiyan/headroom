# Working on headroom

headroom answers which of this machine's Claude Code and Codex accounts has
quota left and which session to get back into, on which account, and
launches the vendor on the answer. A person reaches it through the board and
shell launchers, agents through the two skills in `skills/`, and other
programs through the contracts below. `DESIGN.md` owns the design: the mental
model, the reverse-engineered vendor contracts and, in § The code, which
package owns what. Read it before changing behaviour, and § A second vendor:
Codex before touching anything vendor-scoped. `README.md` is the user's story,
`docs/REFERENCE.md` what crosses commands, and `headroom <command> --help` each
command's own reference.

- One maintainer's tool: each piece of work on its own branch, merged to
  `main` and pushed at milestones; keep changes proportional to demonstrated
  failures or requested features. Dependencies: the standard library, clikit
  and Kong, plus `golang.org/x/term` for the pickers' raw mode. No cgo.
- **Contracts other repositories depend on**, pinned by `TestContracts`
  against `cmd/headroom/testdata/contract/` (rewritten only with
  `HEADROOM_CONTRACT_UPDATE=1`). A change to one updates its consumers in the
  same pass.
  - `version` → planlab steward's `install.sh`, `ship.sh`, `steward.ts`: the
    commit is the second word of line 1.
  - `resolve <name>` → steward's `steward-login.sh`, `steward.ts`: one
    `name<TAB>dir<TAB>primary|extra` line.
  - `limits [--vendor claude]`, and `accounts --compact` off a terminal →
    dotfiles' quota mod and `claude-quota-refresh.sh`, `steward.ts`: schema
    7's fields, one frame, exit 0.
  - The launch's stderr line → steward's `turn_runner.ts` regex:
    `headroom launch: <account> · auto|pinned|…`.
  - `launch [flags] -- <args>` → the dotfiles zsh and codex wrappers, envoy,
    brief, gazette, steward: a bare trailing `--`, a second `--` reaching the
    vendor, `--vendor` after `--` belonging to the vendor.
  - `check`'s exit codes → `steward.ts`: 0 passed, 2 untested, anything else
    failed.
  - `headless place`, `token`, `started` → skill-evals (its contract is
    `docs/specs/headless.md`): JSON on stdout at exit 0 and 3, `--exclude`
    repeated.
  - `login start`, `finish`, `status`, `cancel` and `login --dry-run --json`
    → the owner's renewal agent and `skills/headroom` (its contract is
    `docs/specs/login-handoff.md`): one JSON document per outcome naming
    the account, `reason` a stable token, exit 3 for refused with nothing
    spent or sent, the code on `finish`'s stdin.
  - `accounts add … --share-config[=<dir>]` → `claude.zsh`, `codex.zsh`,
    `steward-login.sh`; `accounts ledger <root>` → steward's `install.sh`,
    under `set -e`.
  - `refresh --vendor <v>` → the detached child a launch starts, and the quota
    refresh. `headroom <url>` with `HEADROOM_BROWSER_*` set → the vendor's
    login, which runs headroom as `$BROWSER`; it is handled before the grammar.

  A command's name, flags or output wording is also what `skills/` teaches: a
  change to one is a change to them.
- **Vendor variation is scope data plus three document readers** —
  identity at discovery, access at preparation, the usage body through
  `usage.Parse`. What a vendor supports as a command is decided by the command
  that owns it, never by a scope flag. DESIGN.md § A second vendor: Codex.
- **Identity travels with every observation.** A usage body naming another
  account or user is unparseable and never shown, and Codex usage is never
  derived from rollouts. Held by `TestCodexResponseNamingSomeoneElseIsNeverShown`
  and `TestCodexRolloutsAreNeverUsage`.
- **Health, Observation and Attempt are three axes, never one status.** A 429
  or an aged-out token annotates what is known and never erases it, and rows
  never travel without their timestamp. Held by `TestApplyWritesEachAxisOnItsOwn`;
  DESIGN.md § Three axes.
- **Only a passed `refreshTokenExpiresAt` calls for a login**, and only on
  positive evidence; `expiresAt` ageing out is routine and any session
  refreshes it. Held by `TestReloginRequired`.
- **One parser per vendor document type**, shared by rendering and `check`.
  Attribution compares decoded fields, never substrings, and a degraded field
  carries a `tag.State` and renders visibly (`?`), never as `0%`. Held by
  `TestDriftTags`.
- **`state.Claim` is the single authorization for a request**: eligibility is
  decided and recorded under one lock, backoff means no traffic, and nothing
  retries inside a run. Held by `TestClaimIsTestAndSet`; DESIGN.md § The one
  file headroom writes.
- **Placement is one rule, chosen and recorded under one lock, fed only from
  disk.** Every launch goes through `state.Place`, a forced one included. A
  change to what the rule chooses takes a new `placement.Rule` name and
  re-freezes its lines (`go test ./internal/eval -run TestFrozen -update`), and
  `headroom launches --eval` judges it before and after. Held by
  `TestPlaceIsChooseAndRecord` and `TestFrozenLinesOfThisRuleAreDecidedAlike`;
  DESIGN.md § Automatic placement.
- **A launched child's environment is a total function of the validated
  decision** (`config.EnvPolicy`, applied through `launch.Target`'s
  constructors). Dirs crossing the seam are absolute, and no surface mints a
  primary decision from corrupt routing state. Held by `TestEnv`,
  `TestCodexEnv` and `TestExtraAndForRefuseRelativeDirs`; DESIGN.md § The
  launch surface.
- **A reservation says what an account may be used for, never that it goes
  first.** It is resolved before the rule into an ordinary exclusion, binds
  automatic choices only, and is never released by a missing directory or an
  unreadable `.reserved`. Held by
  `TestEveryAutomaticSurfaceHonoursAReservation`,
  `TestAMissingRepositoryKeepsItsAccountHeld` and
  `TestADamagedReservationRefusesOnlyAutomaticChoices`; DESIGN.md § Automatic
  placement.
- **A session's owner is evidence, newest wins, never re-stamped.** Resume
  never writes `.current`, and a session named by explicit id follows its
  owner, with any move recorded. Held by `TestResolveOwner` and
  `TestANamedSessionFollowsItsOwner`; DESIGN.md § The session surface.
- **One vendor login per account dir, and a code sent once.** Whatever runs
  `claude auth login` — the batch or a handed-off sign-in's holder — holds
  the dir's login lock and registers the vendor from its launch, and a
  vendor runs only while registered; a holder sends a code only after
  `exchanging` is on disk. Held by `TestHandoffAndTheBatchExcludeEachOther`,
  `TestHandoffBinaryEndsAnyVendorLeftBehind` and
  `TestHandoffSendsNoCodeItCannotRecord`; DESIGN.md § Renewing from
  elsewhere.
- **Read-only against the vendors** except three explicit commands — the
  picker's `r` and `dd`, and `accounts remove`'s Keychain item — each refused
  while liveness is unverifiable. No Codex file is ever written, and the usage
  client only issues GETs. DESIGN.md § The system observed.
- **A second home shares the subscription, never a dir.** No login file is
  linked between dirs, and a ledger root is never a pointer. Held by
  `TestNoLoginIsLinkedBetweenDirs` and `TestALedgerRootMustNotDelegate`;
  DESIGN.md § A second home.
- **Accounts fail independently**: a per-account problem is that account's
  status line, never a process failure. `check` tells broken (exit 1) from
  couldn't test (exit 2), and its own files from the vendor's.
- Tests never touch live state. That means the Keychain, the vendors' dirs and
  usage endpoints, the user's `.current`, and live session registries. Each
  test uses a temporary `HEADROOM_HOME`, with stub `claude`, `codex`,
  `security` and `open` on a `PATH` without the real ones, and usage URLs on a
  local server or `127.0.0.1:1`; `TestMain` clears the environment a managed
  shell carries. `make test-pty` drives the pickers through a real pty
  (`test/pty/run.sh`).
- Ship: `make check`, then `make install`. Callers run `~/.local/bin/headroom`
  and keep the old build until then; the mini keeps its own until
  `twin sync headroom`.
- The command line runs on clikit: how help renders and how argv errors read
  are clikit's (`~/dev/clikit`). Help, results and errors an agent reads
  follow `~/dotfiles/claude/.claude/skills/prompt-engineering/SKILL.md`;
  library questions go to `~/.agents/skills/find-docs/SKILL.md`.

Docs follow `~/dotfiles/docs/documentation-standards.md`, paths written from
the repository root. A command change lands in its help page (`make golden`),
in `docs/REFERENCE.md` where it crosses commands, and in `skills/`.
