package main

import (
	"errors"
	"slices"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"

	"github.com/qiushiyan/headroom/internal/config"
)

// CLI is headroom's command line. Bare, it is the board.
type CLI struct {
	Version kong.VersionFlag `help:"Print the build this binary is — its commit and the commit's time — and exit."`

	Board    BoardCmd    `cmd:"" default:"withargs" help:"The account board: every account's limits, live; also headroom accounts."`
	Accounts AccountsCmd `cmd:"" help:"The account board, and adding, removing and sharing accounts."`
	Launch   LaunchCmd   `cmd:"" help:"Run claude, or codex, on the account headroom decides."`
	Sessions SessionsCmd `cmd:"" help:"Continue any Claude Code session on this machine, on the account that last drove it."`
	Limits   LimitsCmd   `cmd:"" help:"Print what is known about every account's limits, read from disk alone."`
	Launches LaunchesCmd `cmd:"" help:"List the newest launches, or judge how automatic placement has done."`
	Login    LoginCmd    `cmd:"" help:"Renew the Claude Code logins that end soon."`
	Headless HeadlessCmd `cmd:"" help:"Place sessions a headless runner starts itself, on per-account tokens."`
	Resolve  ResolveCmd  `cmd:"" help:"Print an account's name, dir and kind, for shell preflight."`
	Refresh  RefreshCmd  `cmd:"" help:"Ask the usage endpoint about every account that may be asked, and store the answers."`
	Check    CheckCmd    `cmd:"" help:"Verify the vendor facts headroom relies on, and its own state."`
	VersionC VersionCmd  `cmd:"" name:"version" help:"Print the build this binary is."`

	Resume ResumeCmd `cmd:"" hidden:"" passthrough:""`
}

func (CLI) Help() string { return rootHelp }

func (CLI) HelpSections() []clikit.Section { return []clikit.Section{configuration} }

// vendorFilter narrows a report to one vendor.
type vendorFilter struct {
	Vendor *config.Vendor `enum:"claude,codex" help:"Only this vendor's accounts; every vendor on this machine when absent."`
}

// scopes are the vendors the report covers.
func (v vendorFilter) scopes(cfg config.Config) ([]config.Scope, error) {
	return filterScopes(cfg, v.Vendor)
}

func filterScopes(cfg config.Config, v *config.Vendor) ([]config.Scope, error) {
	if v == nil {
		return cfg.Present(), nil
	}
	s := cfg.Scope(*v)
	return []config.Scope{s}, s.RequirePresent()
}

// vendorOne picks the vendor a command acts on.
type vendorOne struct {
	Vendor config.Vendor `enum:"claude,codex" default:"claude" help:"The vendor whose account this acts on."`
}

func (v vendorOne) scope(cfg config.Config) (config.Scope, error) {
	s := cfg.Scope(v.Vendor)
	return s, s.RequirePresent()
}

// accountName is an account named on the command line, by email or dir name.
// An empty one is refused: it is a caller's expansion bug
// (`--account "$broken"`), never a request for the pinned account, which is
// what an absent name means.
type accountName string

func (a *accountName) Decode(ctx *kong.DecodeContext) error {
	var s string
	if err := ctx.Scan.PopValueInto("account", &s); err != nil {
		return err
	}
	if s == "" {
		return errors.New("an account name cannot be empty")
	}
	*a = accountName(s)
	return nil
}

func names(as []accountName) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = string(a)
	}
	return out
}

// shareConfig is --share-config: bare, or --share-config=<dir>. It never
// takes the next word, so a bare flag before the email leaves the email the
// email.
type shareConfig struct {
	set bool
	dir string
}

func (s *shareConfig) IsBool() bool { return true }

func (s *shareConfig) Decode(ctx *kong.DecodeContext) error {
	s.set = true
	if t := ctx.Scan.Peek(); t.Type == kong.FlagValueToken {
		ctx.Scan.Pop()
		s.dir = t.String()
		if s.dir == "" {
			return errors.New("--share-config= needs a directory")
		}
	}
	return nil
}

// given reports whether the flag named name was on the command line, as
// opposed to holding its default.
func given(ctx *kong.Context, name string) bool {
	return slices.ContainsFunc(ctx.Path, func(p *kong.Path) bool { return p.Flag != nil && p.Flag.Name == name })
}

// vendorArgs are the arguments a passthrough positional collected, with the
// `--` that ended headroom's own flags removed: Kong keeps it as the first
// element.
func vendorArgs(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}
