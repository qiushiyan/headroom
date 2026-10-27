package main

import (
	"fmt"
	"io"
	"math"
	"time"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"

	"github.com/qiushiyan/headroom/internal/app"
	"github.com/qiushiyan/headroom/internal/check"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/render"
)

// boardFlags are the board's presentation.
type boardFlags struct {
	Compact bool `xor:"form" help:"One row per account: each limit's percent and time to reset, every warning at the end of the row."`
	JSON    bool `name:"json" xor:"form" help:"Print the board as a JSON document after one refresh round."`
}

func (b boardFlags) run(ctx *kong.Context, in io.Reader, term app.Terminal, h host, scopes []config.Scope) error {
	std := streams(ctx, in)
	deps := app.BoardDeps{Round: h.round(true), Place: h.place}
	if b.JSON {
		return outcome(app.BoardJSON(std, scopes, deps))
	}
	layout := render.LayoutBlocks
	if b.Compact {
		layout = render.LayoutCompact
	}
	return outcome(app.Board(std, term, scopes, layout, deps))
}

type BoardCmd struct {
	vendorFilter
	boardFlags
}

func (BoardCmd) Help() string                   { return boardHelp }
func (BoardCmd) HelpSections() []clikit.Section { return []clikit.Section{boardDocument} }

func (c *BoardCmd) Run(ctx *kong.Context, in io.Reader, term app.Terminal, cfg config.Config, h host) error {
	scopes, err := c.scopes(cfg)
	if err != nil {
		return err
	}
	return c.run(ctx, in, term, h, scopes)
}

type AccountsCmd struct {
	Vendor *config.Vendor `enum:"claude,codex" help:"The vendor whose accounts these are: the board shows only its; add and remove act on it, Claude Code when absent."`

	Board  AccountsBoardCmd `cmd:"" default:"withargs" help:"The account board."`
	Add    AddCmd           `cmd:"" help:"Seed the dir for a new subscription."`
	Remove RemoveCmd        `cmd:"" help:"Remove an extra account: its Keychain item and its dir."`
	Ledger LedgerCmd        `cmd:"" help:"Spend against another home's ledger, and list the homes that share it."`
}

func (AccountsCmd) Help() string { return accountsHelp }

// one is the vendor add and remove act on.
func (c *AccountsCmd) one(cfg config.Config) config.Scope {
	if c.Vendor == nil {
		return cfg.Claude
	}
	return cfg.Scope(*c.Vendor)
}

type AccountsBoardCmd struct {
	boardFlags
}

func (AccountsBoardCmd) Help() string                   { return boardHelp }
func (AccountsBoardCmd) HelpSections() []clikit.Section { return []clikit.Section{boardDocument} }

func (c *AccountsBoardCmd) Run(ctx *kong.Context, in io.Reader, term app.Terminal, cfg config.Config, h host, parent *AccountsCmd) error {
	scopes, err := filterScopes(cfg, parent.Vendor)
	if err != nil {
		return err
	}
	return c.run(ctx, in, term, h, scopes)
}

type AddCmd struct {
	Email       string      `arg:"" help:"The new subscription's email: the account's dir name, and the login it must hold."`
	ShareConfig shareConfig `name:"share-config" help:"Link the primary's config into the new dir; --share-config=<dir> links every entry of <dir> instead."`
}

func (AddCmd) Help() string { return addHelp }

func (c *AddCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config, parent *AccountsCmd) error {
	// The one command that works while its vendor is absent: it is how the
	// first Codex directory comes to exist.
	return outcome(app.AccountsAdd(streams(ctx, in), parent.one(cfg), app.AddOptions{
		Email: c.Email, ShareConfig: c.ShareConfig.set, ShareFrom: c.ShareConfig.dir,
	}))
}

type RemoveCmd struct {
	// A plain string: an empty name is the bare form, as it always was.
	Account string `arg:"" optional:"" help:"The account to remove, or <name>.lock debris; a picker on a terminal when absent."`
	Yes     bool   `short:"y" help:"Remove without asking."`
}

func (RemoveCmd) Help() string { return removeHelp }

func (c *RemoveCmd) Run(ctx *kong.Context, in io.Reader, term app.Terminal, cfg config.Config, h host, parent *AccountsCmd) error {
	scope := parent.one(cfg)
	if err := scope.RequirePresent(); err != nil {
		return err
	}
	return outcome(app.AccountsRemove(streams(ctx, in), term, scope, c.Account, c.Yes, h.place.Probe))
}

type LedgerCmd struct {
	Root string `arg:"" optional:"" help:"The accounts root whose ledger this home spends against; this home's own stops sharing."`
}

func (LedgerCmd) Help() string { return ledgerHelp }

func (c *LedgerCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config, parent *AccountsCmd) error {
	if parent.Vendor != nil && *parent.Vendor != config.Claude {
		return clikit.Usage(fmt.Errorf("only Claude Code homes share a ledger"))
	}
	return outcome(app.AccountsLedger(streams(ctx, in), cfg, c.Root, time.Now()))
}

type LaunchCmd struct {
	vendorOne
	Auto     bool        `xor:"decide" help:"Place this one launch on the least-loaded account, whatever the board pins."`
	Last     bool        `xor:"decide,pin" help:"Launch on the account of the newest recorded launch."`
	Account  accountName `xor:"decide" placeholder:"NAME" help:"Launch on this account, named by email or dir name."`
	Remember bool        `xor:"once,pin" help:"Record the decision for later bare launches: the account, or with --auto the automatic mode."`
	DryRun   bool        `xor:"once" help:"Print the choice and every account's figures; record, log and start nothing."`
	Args     []string    `arg:"" optional:"" passthrough:"partial" help:"Arguments for claude (or codex), passed on unchanged; put them after --."`
}

func (LaunchCmd) Help() string                   { return launchHelp }
func (LaunchCmd) HelpSections() []clikit.Section { return []clikit.Section{launchExamples} }

func (c *LaunchCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config, h host) error {
	scope, err := c.scope(cfg)
	if err != nil {
		return err
	}
	return outcome(app.Launch(streams(ctx, in), scope, app.LaunchOptions{
		Account: string(c.Account), Auto: c.Auto, Last: c.Last, Remember: c.Remember, DryRun: c.DryRun,
		Args: vendorArgs(c.Args),
	}, h.place, h.exec))
}

type SessionsCmd struct {
	JSON   bool     `name:"json" help:"List the sessions as JSON instead of picking one; takes nothing else."`
	CDFile string   `name:"cd-file" placeholder:"PATH" help:"An absolute path to write the entered project dir to, for the shell's own cd once claude exits."`
	Args   []string `arg:"" optional:"" passthrough:"partial" help:"Arguments for claude, before the picker's own --resume; put them after --."`
}

func (SessionsCmd) Help() string { return sessionsHelp }

func (c *SessionsCmd) Run(ctx *kong.Context, in io.Reader, term app.Terminal, cfg config.Config, h host) error {
	args := vendorArgs(c.Args)
	if c.JSON && (c.CDFile != "" || len(args) > 0) {
		return clikit.Usage(fmt.Errorf("--json takes no other arguments"))
	}
	return outcome(app.Sessions(streams(ctx, in), term, cfg.Claude,
		app.SessionsOptions{JSON: c.JSON, CDFile: c.CDFile, Args: args},
		app.SessionsDeps{Place: h.place, Exec: h.exec}))
}

type LimitsCmd struct {
	vendorFilter
	Account accountName `placeholder:"NAME" help:"Only the accounts of this name, in every vendor selected."`
}

func (LimitsCmd) Help() string                   { return limitsHelp }
func (LimitsCmd) HelpSections() []clikit.Section { return []clikit.Section{boardDocument} }

func (c *LimitsCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	scopes, err := c.scopes(cfg)
	if err != nil {
		return err
	}
	return outcome(app.Limits(streams(ctx, in), scopes, string(c.Account)))
}

type LaunchesCmd struct {
	vendorFilter
	Count int    `short:"n" default:"20" help:"How many of the newest launches to list; 0 lists every one."`
	JSON  bool   `name:"json" help:"Print the records, or with --eval the evaluation, as JSON."`
	Eval  bool   `help:"Judge how automatic placement has done instead of listing launches."`
	Since string `placeholder:"SPAN|DATE" help:"With --eval, only what happened since: a span back from now (7d, 36h) or a date (2026-10-01)."`
}

func (LaunchesCmd) Help() string { return launchesHelp }

func (c *LaunchesCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	switch {
	case c.Eval && given(ctx, "count"):
		return clikit.Usage(fmt.Errorf("--eval reads every line — -n does not apply; --since narrows it"))
	case !c.Eval && c.Since != "":
		return clikit.Usage(fmt.Errorf("--since narrows --eval"))
	case c.Count < 0:
		return clikit.Usage(fmt.Errorf("-n %d is not a count", c.Count))
	}
	scopes, err := c.scopes(cfg)
	if err != nil {
		return err
	}
	std, now := streams(ctx, in), time.Now()
	if !c.Eval {
		return outcome(app.Launches(std, scopes, c.Count, c.JSON))
	}
	var since time.Time
	if c.Since != "" {
		if since, err = app.ParseSince(c.Since, now); err != nil {
			return clikit.Usage(err)
		}
	}
	return outcome(app.LaunchesEval(std, scopes, since, c.JSON, now))
}

type LoginCmd struct {
	Renew  LoginRenewCmd  `cmd:"" default:"withargs" help:"Renew the logins that end soon, or the named ones, one after another at this terminal."`
	Start  LoginStartCmd  `cmd:"" help:"Start one account's sign-in for an approver elsewhere, and print its URL as JSON."`
	Finish LoginFinishCmd `cmd:"" help:"Send a started sign-in the code its page showed, from stdin, and print the verdict as JSON."`
	Status LoginStatusCmd `cmd:"" help:"Print one account's started sign-in as JSON."`
	Cancel LoginCancelCmd `cmd:"" help:"End one account's started sign-in while it waits for its code."`
	Hold   LoginHoldCmd   `cmd:"" hidden:"" help:"The holder login start runs, detached; not for use by hand."`
}

func (LoginCmd) Help() string { return loginHelp + "\n\n" + handoffHelp }

type LoginRenewCmd struct {
	Accounts []accountName `arg:"" name:"account" optional:"" sep:"none" help:"The accounts to renew, by email or dir name; bare, the ones that end soon."`
	All      bool          `help:"Renew every account's login, so they end together."`
	Within   int           `default:"7" placeholder:"DAYS" help:"Renew the logins that end within this many days."`
	DryRun   bool          `help:"Print the plan and renew nothing."`
	JSON     bool          `name:"json" help:"With --dry-run, print the plan as JSON."`
}

func (LoginRenewCmd) Help() string { return loginHelp }

// maxDays is the most days a duration can count.
const maxDays = math.MaxInt64 / int64(24*time.Hour)

func (c *LoginRenewCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	switch {
	case c.All && len(c.Accounts) > 0:
		return clikit.Usage(fmt.Errorf("--all and account names exclude each other"))
	case c.Within < 0 || int64(c.Within) > maxDays:
		return clikit.Usage(fmt.Errorf("--within %d is not a number of days", c.Within))
	case c.JSON && !c.DryRun:
		return clikit.Usage(fmt.Errorf("--json prints the plan, with --dry-run; to renew without a terminal, headroom login start and headroom login finish"))
	}
	return outcome(app.Login(streams(ctx, in), cfg.Claude, app.LoginOptions{
		Names: names(c.Accounts), All: c.All, Within: time.Duration(c.Within) * 24 * time.Hour, DryRun: c.DryRun, JSON: c.JSON,
	}))
}

type LoginStartCmd struct {
	Account accountName `arg:"" help:"The account whose login to renew, by email or dir name."`
}

func (LoginStartCmd) Help() string { return loginStartHelp }

func (c *LoginStartCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.LoginStart(streams(ctx, in), cfg.Claude, string(c.Account)))
}

type LoginFinishCmd struct {
	Account accountName `arg:"" help:"The account whose sign-in the code is for."`
}

func (LoginFinishCmd) Help() string { return loginFinishHelp }

func (c *LoginFinishCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.LoginFinish(streams(ctx, in), cfg.Claude, string(c.Account)))
}

type LoginStatusCmd struct {
	Account accountName `arg:"" help:"The account whose sign-in to print."`
}

func (LoginStatusCmd) Help() string {
	return `Prints the account's sign-in as start and finish print it: awaiting_code or exchanging while
it is live, and how it ended after — kept until the next start of the account; none when no
sign-in was ever started. It exits 0.`
}

func (c *LoginStatusCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.LoginStatus(streams(ctx, in), cfg.Claude, string(c.Account)))
}

type LoginCancelCmd struct {
	Account accountName `arg:"" help:"The account whose sign-in to end."`
}

func (LoginCancelCmd) Help() string {
	return `Ends the account's sign-in while it waits for its code, and prints it: state failed, reason
cancelled. It exits 0, also when there was nothing to end, and 3 while a code is being
exchanged, which cannot be called back. Then headroom login start begins afresh.`
}

func (c *LoginCancelCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.LoginCancel(streams(ctx, in), cfg.Claude, string(c.Account)))
}

type LoginHoldCmd struct {
	Account accountName `arg:"" help:"The account whose sign-in to hold."`
}

func (c *LoginHoldCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.LoginHold(streams(ctx, in), cfg.Claude, string(c.Account)))
}

type HeadlessCmd struct {
	List    HeadlessListCmd    `cmd:"" default:"withargs" help:"List every account's headless token: whether there is one, and when it ends."`
	Mint    HeadlessMintCmd    `cmd:"" help:"Mint a long-lived token for one account through claude setup-token."`
	Place   HeadlessPlaceCmd   `cmd:"" help:"Decide which account the next headless session spends, and record it."`
	Started HeadlessStartedCmd `cmd:"" help:"Report the process a session placed by place runs as, so it counts while it runs."`
	Token   HeadlessTokenCmd   `cmd:"" help:"Print one account's headless token, and nothing else."`
	Forget  HeadlessForgetCmd  `cmd:"" help:"Forget one account's headless token."`
}

func (HeadlessCmd) Help() string { return headlessHelp }

type HeadlessListCmd struct {
	JSON bool `name:"json" help:"Print the listing as JSON."`
}

func (HeadlessListCmd) Help() string { return headlessHelp }

func (c *HeadlessListCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.HeadlessList(streams(ctx, in), cfg.Claude, c.JSON, time.Now()))
}

type HeadlessMintCmd struct {
	Account accountName `arg:"" help:"The account to mint for; its dir must be logged in to it."`
}

func (HeadlessMintCmd) Help() string {
	return `Mints a long-lived token for one account through Claude Code's own claude setup-token,
approved in the Chrome profile matched to the account — or, over ssh, at the printed URL
with the code pasted back — and stores it in a file only you can read. It needs a terminal:
the vendor asks for the approval there, and waits up to ten minutes for it.`
}

func (c *HeadlessMintCmd) Run(ctx *kong.Context, in io.Reader, term app.Terminal, cfg config.Config) error {
	return outcome(app.HeadlessMint(streams(ctx, in), term, cfg.Claude, string(c.Account)))
}

type HeadlessPlaceCmd struct {
	Prefer  accountName   `placeholder:"NAME" help:"The account the session ran on last, followed while it has room so its prompt cache stays warm."`
	Exclude []accountName `sep:"none" placeholder:"NAME" help:"An account not to take — one whose session ended on a usage limit, or whose token the vendor refused; repeat for more."`
	DryRun  bool          `help:"Decide, and record nothing."`
}

func (HeadlessPlaceCmd) Help() string { return placeHelp }

func (c *HeadlessPlaceCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config, h host) error {
	return outcome(app.HeadlessPlace(streams(ctx, in), cfg.Claude, app.HeadlessPlaceOptions{
		Prefer: string(c.Prefer), Exclude: names(c.Exclude), DryRun: c.DryRun,
	}, h.place))
}

type HeadlessStartedCmd struct {
	Placement string `arg:"" help:"The placement id place printed."`
	PID       int    `arg:"" name:"pid" help:"The process the session runs as."`
}

func (c *HeadlessStartedCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config, h host) error {
	switch {
	case c.Placement == "":
		return clikit.Usage(fmt.Errorf("a placement id cannot be empty"))
	case c.PID <= 0:
		return clikit.Usage(fmt.Errorf("%d is not a process id", c.PID))
	}
	return outcome(app.HeadlessStarted(streams(ctx, in), cfg.Claude, c.Placement, c.PID, h.place))
}

type HeadlessTokenCmd struct {
	Account accountName `arg:"" help:"The account whose token to print."`
}

func (c *HeadlessTokenCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.HeadlessToken(streams(ctx, in), cfg.Claude, string(c.Account)))
}

type HeadlessForgetCmd struct {
	Account accountName `arg:"" help:"The account whose token to forget; headroom does not revoke it with the vendor."`
}

func (c *HeadlessForgetCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	return outcome(app.HeadlessForget(streams(ctx, in), cfg.Claude, string(c.Account)))
}

type ResolveCmd struct {
	vendorOne
	Account accountName `arg:"" optional:"" help:"The account to resolve; the pinned one when absent."`
}

func (ResolveCmd) Help() string { return resolveHelp }

func (c *ResolveCmd) Run(ctx *kong.Context, in io.Reader, cfg config.Config) error {
	scope, err := c.scope(cfg)
	if err != nil {
		return err
	}
	return outcome(app.Resolve(streams(ctx, in), scope, string(c.Account)))
}

type RefreshCmd struct {
	vendorFilter
}

func (RefreshCmd) Help() string { return refreshHelp }

func (c *RefreshCmd) Run(cfg config.Config, h host) error {
	scopes, err := c.scopes(cfg)
	if err != nil {
		return err
	}
	return outcome(app.Refresh(scopes, h.round(false)))
}

type CheckCmd struct{}

func (CheckCmd) Help() string { return checkHelp }

// Run loads the configuration itself: one headroom refuses is not an
// assertion it could not test but its own state, broken, and every launch
// refuses on it right now — said as the FAIL it is.
func (CheckCmd) Run(ctx *kong.Context, term app.Terminal, h host) error {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(ctx.Stdout, "FAIL  config: headroom refuses its configuration — every command refuses until it is fixed")
		return clikit.Fail(check.ExitFail, err)
	}
	return clikit.Exit(check.Run(cfg, ctx.Stdout, term.Out, h.place.Probe))
}

type VersionCmd struct{}

func (VersionCmd) Run(ctx *kong.Context) error {
	fmt.Fprintln(ctx.Stdout, app.VersionLine())
	return nil
}

// ResumeCmd is the retired `resume`, dispatched before configuration and
// taking anything, --help included: the shell it diagnoses is stale, and
// stale shells come with old environments that configuration may now refuse,
// so this message must speak regardless. The name must never be rebound: its
// stdout was a decision protocol, and one name with two meanings across
// binary generations is how a session once started on the wrong account.
// Nothing goes to stdout.
type ResumeCmd struct {
	Args []string `arg:"" optional:"" help:"Whatever the stale shell passes; ignored."`
}

func (ResumeCmd) Run(ctx *kong.Context) error {
	fmt.Fprint(ctx.Stderr, "headroom: `resume` was retired. Its stdout was a decision protocol, and a\n"+
		"shell function loaded before this binary can misread it — that produced a\n"+
		"session on the wrong account.\n"+
		"Your shell integration is stale. Fix it:  exec zsh\n"+
		"The session picker is now `headroom sessions` (listing: `headroom sessions --json`).\n")
	return clikit.Exit(2)
}
