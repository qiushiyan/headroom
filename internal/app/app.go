// Package app wires the pieces: discover accounts, resolve credentials,
// fetch usage in parallel, render. Any per-account problem becomes a status
// rendered for that account alone — accounts fail independently.
package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/qiushiyan/headroom/internal/accounts"
	"github.com/qiushiyan/headroom/internal/check"
	"github.com/qiushiyan/headroom/internal/config"
	"github.com/qiushiyan/headroom/internal/creds"
	"github.com/qiushiyan/headroom/internal/render"
	"github.com/qiushiyan/headroom/internal/usage"
)

func Run(args []string) int {
	cfg := config.Load()
	cmd := ""
	var rest []string
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	// Every command but watch takes no further arguments; a stray one is an
	// error, not silently ignored — a misspelled flag must not fall through
	// to a command it wasn't meant for.
	noArgs := func() bool {
		if len(rest) == 0 {
			return true
		}
		fmt.Fprintf(os.Stderr, "headroom: unexpected argument %q\n", rest[0])
		printUsage(os.Stderr)
		return false
	}
	switch cmd {
	case "":
		return runDashboard(cfg)
	case "--json":
		if !noArgs() {
			return 2
		}
		return runDashboardJSON(cfg)
	case "check", "--check":
		if !noArgs() {
			return 2
		}
		return check.Run(cfg, os.Stdout, stdoutIsTTY())
	case "select":
		if !noArgs() {
			return 2
		}
		return runSelect(cfg)
	case "watch":
		return runWatch(cfg, rest)
	case "-h", "--help", "help":
		if !noArgs() {
			return 2
		}
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "headroom: unknown command %q\n", cmd)
		printUsage(os.Stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: headroom [command]

  (none)   usage dashboard for every account
  --json   the dashboard as JSON (schema versioned)
  select   interactively pick the account bare x targets
  watch    the dashboard on a lazy refresh loop (--interval <duration>)
  check    verify the reverse-engineered assumptions still hold
`)
}

func stdoutIsTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// accountData carries one account through the prepare → fetch → render
// pipeline.
type accountData struct {
	Acct       accounts.Account
	View       render.AccountView
	Token      string
	NeedsFetch bool
}

// prepare walks accounts and resolves labels, launchers, and credentials;
// fetch-ready accounts are left in StatusPending for launchFetches. It also
// returns the current-target name it marked the views with — consumers that
// report the current account must use this value, not re-read the state
// file, or a concurrent `select` could make the two disagree.
func prepare(cfg config.Config) ([]*accountData, string) {
	return prepareWith(cfg, creds.ReadRaw)
}

// prepareWith is prepare with the credential source injected — the one seam
// that lets the pipeline be table-tested without a Keychain.
func prepareWith(cfg config.Config, readRaw func(configDir string) string) ([]*accountData, string) {
	accts := accounts.Discover(cfg)
	current := accounts.CurrentTarget(cfg)
	nowMS := time.Now().UnixMilli()

	list := make([]*accountData, 0, len(accts))
	for _, a := range accts {
		d := &accountData{Acct: a}
		list = append(list, d)

		v := &d.View
		v.Label = a.Name
		if a.Email != "" {
			v.Label = a.Email
		}
		if !a.IsPrimary() && a.Email != "" && a.Email != a.Name {
			v.DirMismatch = a.Name
		}
		v.Launcher = accounts.Launcher(a, accts, cfg.PrimaryName)
		v.Current = current == a.Name

		raw := readRaw(a.ConfigDir)
		if raw == "" {
			v.Status = render.StatusNoLogin
			continue
		}
		blob, ok := creds.Parse(raw)
		if !ok {
			v.Status = render.StatusBadBlob
			continue
		}
		v.Plan = blob.PlanLabel()
		if blob.Expired(nowMS) {
			v.Status = render.StatusExpired
			continue
		}
		d.Token = blob.Token
		d.NeedsFetch = true
		v.Status = render.StatusPending
	}
	return list, current
}

// fetchUpdate is one finished fetch, addressed by account index. The
// receiver applies it with resolve.
type fetchUpdate struct {
	idx int
	res usage.Result
}

// launchFetches starts all usage fetches in parallel; the channel closes
// when all are done. Fetch goroutines never touch a view — the caller
// applies each result with resolve, so views have exactly one writer and a
// redraw may read every view between receives. The buffer lets senders
// finish even if the caller stops receiving early.
func launchFetches(ctx context.Context, cfg config.Config, list []*accountData) <-chan fetchUpdate {
	client := &http.Client{Timeout: 10 * time.Second}
	updates := make(chan fetchUpdate, len(list))
	var wg sync.WaitGroup
	for i, d := range list {
		if !d.NeedsFetch {
			continue
		}
		wg.Add(1)
		go func(i int, token string) {
			defer wg.Done()
			updates <- fetchUpdate{i, usage.Fetch(ctx, client, cfg.UsageURL, token)}
		}(i, d.Token)
	}
	go func() {
		wg.Wait()
		close(updates)
	}()
	return updates
}

func resolve(d *accountData, res usage.Result) {
	v := &d.View
	switch {
	case res.Err != nil:
		v.Status = render.StatusFetchFailed
	case res.StatusCode != http.StatusOK:
		v.Status = render.StatusHTTPError
		v.HTTPCode = res.StatusCode
	default:
		rows, err := usage.ParseLimits(res.Body)
		switch {
		case err != nil:
			v.Status = render.StatusUnparseable
		case len(rows) == 0:
			v.Status = render.StatusNoLimits
		default:
			v.Rows = rows
			v.Status = render.StatusRows
		}
	}
}

func runDashboard(cfg config.Config) int {
	list, _ := prepare(cfg)
	for u := range launchFetches(context.Background(), cfg, list) {
		resolve(list[u.idx], u.res)
	}
	p := render.NewPalette(stdoutIsTTY())
	now := time.Now().Unix()
	labelWidth := render.LabelWidth(views(list))
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for i, d := range list {
		if i > 0 {
			fmt.Fprintln(out)
		}
		for _, line := range p.AccountBlock(d.View, now, labelWidth) {
			fmt.Fprintln(out, line)
		}
	}
	return 0
}

func views(list []*accountData) []render.AccountView {
	vs := make([]render.AccountView, len(list))
	for i, d := range list {
		vs[i] = d.View
	}
	return vs
}
