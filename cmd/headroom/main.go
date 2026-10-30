// headroom answers which of this machine's Claude Code and Codex accounts has
// quota left, and which session to get back into, on which account — and
// launches the vendor on the answer. This package is its command line: the
// grammar and its help, and one call into internal/app per command. The
// mental model and the vendor contracts are DESIGN.md's.
package main

import (
	"errors"
	"io"
	"os"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"
	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/app"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/launch"
	"github.com/qiushiyan/headroom/internal/launchop"
	"github.com/qiushiyan/headroom/internal/round"
)

var cli = clikit.App{
	Name: "headroom",
	Description: "headroom shows which of this machine's Claude Code and Codex accounts has quota left, " +
		"launches claude or codex on the account it decides, and continues any Claude Code session on " +
		"the account that last drove it.",
	New: func() any { return &CLI{} },
	Options: []kong.Option{
		kong.Vars{"version": app.VersionLine()},
		kong.BindSingletonProvider(loadConfig),
		kong.Bind(liveHost()),
		kong.BindToProvider(terminalOf),
	},
	ExitCodes: map[int]string{
		2: "invalid arguments, nothing was run; for check, an assertion it could not test (INCONCLUSIVE)",
		3: "headless place refused: every account it could take is near a limit or excluded; login start, finish or cancel refused, nothing spent or sent: the document's reason says why",
	},
}

func main() {
	// headroom as a vendor's $BROWSER: one URL, no command, before anything
	// reads configuration or argv.
	if code, ok := app.Open(os.Args[1:], os.Stderr); ok {
		os.Exit(code)
	}
	os.Exit(cli.Run(os.Args[1:]))
}

// loadConfig is every command's configuration, read once per run, with each
// vendor's records first moved to the name its primary answers to now
// (app.SettlePrimaryNames): a host file that renames the primary is all a
// person changes.
func loadConfig(ctx *kong.Context) (config.Config, error) {
	cfg, err := config.Load()
	if err == nil {
		cfg = app.SettlePrimaryNames(cfg, ctx.Stderr)
	}
	return cfg, err
}

// host is the machine a run acts on: the edges a surface reaches through
// beyond its files, bound once. A test replaces it by appending a binding of
// its own to a copy of the App's options.
type host struct {
	place launchop.Deps
	round func(probe bool) round.Deps
	exec  app.ExecFunc
}

func liveHost() host {
	return host{place: app.LivePlace(), round: app.LiveRound, exec: launch.ExecPath}
}

// terminalOf says which of the run's streams reach a terminal: the App's
// own, which are the process's in production and a test's buffers otherwise.
func terminalOf(ctx *kong.Context, in io.Reader) app.Terminal {
	var t app.Terminal
	if f, ok := in.(*os.File); ok {
		t.In = term.IsTerminal(int(f.Fd()))
	}
	if f, ok := ctx.Stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		t.Out = true
		t.Size = func() (int, int, error) { return term.GetSize(int(f.Fd())) }
	}
	return t
}

// streams are the run's: the App's writers and stdin.
func streams(ctx *kong.Context, in io.Reader) app.Streams {
	return app.Streams{In: in, Out: ctx.Stdout, Err: ctx.Stderr}
}

// outcome is a surface's result as the run's status: a status it chose after
// reporting, a request it could not carry out as asked, or a failure.
func outcome(err error) error {
	if s, ok := errors.AsType[app.Status](err); ok {
		return clikit.Exit(int(s))
	}
	if u, ok := errors.AsType[app.UsageError](err); ok {
		return clikit.Usage(u.Err)
	}
	return err
}
