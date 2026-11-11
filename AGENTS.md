# Working on headroom

headroom answers which of this machine's Claude Code and Codex accounts has
quota left and launches the vendor on the answer — a new session on the
least-loaded account, a resumed one on the account that last drove it. A
person reaches it through the board and shell launchers, agents through the
skills in `skills/`, and other programs through the contracts below.
`DESIGN.md` owns the design: the mental model, the reverse-engineered vendor
contracts and, in § The code, which package owns what. Read it before changing behaviour, and § A second vendor:
Codex before touching anything vendor-scoped. `README.md` is the user's story,
`docs/REFERENCE.md` what crosses commands, and `headroom <command> --help` each
command's own reference.

- One maintainer's tool: each piece of work on its own branch, merged to
  `main` and pushed at milestones; keep changes proportional to demonstrated
  failures or requested features. Dependencies: the standard library, clikit
  and Kong, plus `golang.org/x/term` for the board's raw mode. No cgo.
- **Contracts other repositories depend on**, pinned by `TestContracts`
  against `cmd/headroom/testdata/contract/` (rewritten only with
  `HEADROOM_CONTRACT_UPDATE=1`). A change to one updates its consumers in the
  same pass.
  - `version` → planlab steward's `install.sh`, `ship.sh`, `steward.ts`: the
    commit is the second word of line 1.
  - `limits [--vendor claude|codex]`, and `accounts --compact` off a terminal
    → dotfiles' quota mod, `claude-quota-refresh.sh` and `remote-logins`:
    schema 8's fields, one frame, exit 0.
  - `launch [flags] -- <args>` → the dotfiles zsh and codex wrappers and
    `remote-logins`, envoy, brief, gazette: a bare trailing `--`, a second
    `--` reaching the vendor, `--vendor` after `--` belonging to the vendor.
  - `headless place`, `token`, `started`, and `headless --json` → skill-evals,
    and steward's `steward-claude` launcher and doctor, which reach this home
    with `HEADROOM_HOME` (the contract is `docs/specs/headless.md`): JSON on
    stdout at exit 0 and 3, `--exclude` repeated.
  - `login start`, `finish`, `status`, `cancel` and `login --dry-run --json`
    → dotfiles' `remote-logins` and `skills/headroom` (the contract is
    `docs/specs/login-handoff.md`): one JSON document per outcome naming
    the account, `reason` a stable token, exit 3 for refused with nothing
    spent or sent, the code on `finish`'s stdin.
  - `accounts add … --share-config[=<dir>]` → `claude.zsh`, `codex.zsh`.
  - `refresh --vendor <v>` → the detached child a launch starts, and the quota
    refresh. `headroom <url>` with `HEADROOM_BROWSER_*` set → the vendor's
    login, which runs headroom as `$BROWSER`; it is handled before the grammar.
  - The retired verbs `sessions`, `resume`, `resolve`, `launches` and
    `accounts ledger`, and `launch --auto` and `--last`, are unknown words:
    exit 2 and nothing on stdout, before any configuration is read. A stale
    caller stops instead of misreading a reply, so none of those names is
    ever rebound to a new meaning.

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
  disk.** Every launch goes through `state.Place`, a forced one included, and
  its record serves the next launch, never an audit: the launch log is
  appended beside it and read by no code. Held by
  `TestPlaceIsChooseAndRecord`; DESIGN.md § Automatic placement.
- **A model's own limit admits, never ranks.** A launch whose `--model` some
  reading limits on its own (Fable) never runs on an account at 90% or more of
  it or with it unreadable, a named one included; an account with no figure is
  tried, and ranking stays the ordinary rule's. Held by
  `TestALaunchAskingForAModelNeverStartsWhereItsLimitIsSpent`,
  `TestNamingAnAccountDoesNotGetPastAModelsLimit` and
  `TestAModelsLimitAdmitsAndNeverRanks`; DESIGN.md § Automatic placement.
- **One quota document per accounts root, under one lock.** A program with
  its own config dir spends a subscription through a headless token and a
  placement, reaching this home with `HEADROOM_HOME` and reporting its
  process — never through logins of its own, which are a second renewal
  cycle, nor a file shared between homes. DESIGN.md § Headless tokens.
- **Nothing records where a bare launch goes.** Every bare launch is placed,
  naming an account lasts one launch, and the board writes no routing — a
  pinned default was the easiest mistake on the board to make. Held by
  `TestOnlyTheRuleRoutesABareLaunch` and `TestTheBoardHasNoRoutingKeys`;
  DESIGN.md § The launch surface.
- **A launched child's environment is a total function of the validated
  decision** (`config.EnvPolicy`, applied through `launch.Target`'s
  constructors). Dirs crossing the seam are absolute, and no surface mints a
  primary decision nobody made. Held by `TestEnv`,
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
- **A record names an account by its dir, never by what it is called.** The
  primary is `.primary`, a name is translated at the edges, so a pin or a new
  login renames nothing on disk, and `state.Place` refuses a candidate with
  no ref. Held by `TestARenamedPrimaryKeepsItsRecords` and
  `TestAChoiceWithNoRefIsNotRecorded`; DESIGN.md § The one file headroom
  writes.
- **A session's owner is evidence, newest wins, never re-stamped.** A session
  named by explicit id follows its owner, with any move recorded. Held by
  `TestResolveOwner` and `TestANamedSessionFollowsItsOwner`; DESIGN.md
  § Sessions: what a launch reads.
- **One vendor login per account dir, and a code sent once.** Whatever runs
  `claude auth login` — the batch or a handed-off sign-in's holder — holds
  the dir's login lock and registers the vendor from its launch, and a
  vendor runs only while registered; a holder sends a code only after
  `exchanging` is on disk. Held by `TestHandoffAndTheBatchExcludeEachOther`,
  `TestHandoffBinaryEndsAnyVendorLeftBehind` and
  `TestHandoffSendsNoCodeItCannotRecord`; DESIGN.md § Renewing from
  elsewhere.
- **Read-only against the vendors** except one explicit command —
  `accounts remove`'s Keychain item — refused while liveness is
  unverifiable. No Codex file is ever written, and the usage client only
  issues GETs. DESIGN.md § The system observed.
- **No login is linked between dirs**: no share mode, and no config package
  that happens to hold a login, history or registry, makes two dirs one
  login. Held by `TestNoLoginIsLinkedBetweenDirs`; DESIGN.md § The account
  lifecycle.
- **Accounts fail independently**: a per-account problem is that account's
  status line, never a process failure. `check` tells broken (exit 1) from
  couldn't test (exit 2), and its own files from the vendor's.
- Tests never touch live state. That means the Keychain, the vendors' dirs and
  usage endpoints, the user's accounts roots, and live session registries. Each
  test uses a temporary `HEADROOM_HOME`, with stub `claude`, `codex`,
  `security` and `open` on a `PATH` without the real ones, and usage URLs on a
  local server or `127.0.0.1:1`; `TestMain` clears the environment a managed
  shell carries. `make test-pty` drives the board through a real pty
  (`test/pty/run.sh`).
- Ship: `make check`, then `make install`. Callers run `~/.local/bin/headroom`
  and keep the old build until then; the mini keeps its own until
  `twin sync headroom`, and steward runs the build its `versions.json` pins.
- The command line runs on clikit: how help renders and how argv errors read
  are clikit's (`~/dev/clikit`). Help, results and errors an agent reads
  follow `~/dotfiles/claude/.claude/skills/prompt-engineering/SKILL.md`;
  library questions go to `~/.agents/skills/find-docs/SKILL.md`.

Docs follow `~/dotfiles/docs/documentation-standards.md`, paths written from
the repository root. A command change lands in its help page (`make golden`),
in `docs/REFERENCE.md` where it crosses commands, and in `skills/`.
