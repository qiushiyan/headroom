// Package app wires the pieces: discover accounts, resolve credentials,
// fetch usage in parallel, render. Any per-account problem becomes a status
// rendered for that account alone — accounts fail independently.
package app

import (
	"bufio"
	"fmt"
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
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "":
		return runDashboard(cfg)
	case "check", "--check":
		return check.Run(cfg, os.Stdout, stdoutIsTTY())
	case "select":
		return runSelect(cfg)
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "headroom: unknown command %q\n", cmd)
		printUsage(os.Stderr)
		return 2
	}
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `usage: headroom [command]

  (none)   usage dashboard for every account
  select   interactively pick the account bare x targets
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

// prepare walks accounts and resolves labels, launchers, and credentials.
// Fetch-ready accounts are left in StatusPending for launchFetches.
func prepare(cfg config.Config) []*accountData {
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

		raw := creds.ReadRaw(a.ConfigDir)
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
	return list
}

// launchFetches starts all usage fetches in parallel and delivers each
// account's index as its view resolves; the channel closes when all are
// done. Receiving an index synchronizes with that account's writes.
func launchFetches(cfg config.Config, list []*accountData) <-chan int {
	client := &http.Client{Timeout: 10 * time.Second}
	done := make(chan int)
	var wg sync.WaitGroup
	for i, d := range list {
		if !d.NeedsFetch {
			continue
		}
		wg.Add(1)
		go func(i int, d *accountData) {
			defer wg.Done()
			resolve(d, usage.Fetch(client, cfg.UsageURL, d.Token))
			done <- i
		}(i, d)
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
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
	list := prepare(cfg)
	for range launchFetches(cfg, list) {
	}
	p := render.NewPalette(stdoutIsTTY())
	now := time.Now().Unix()
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for i, d := range list {
		if i > 0 {
			fmt.Fprintln(out)
		}
		for _, line := range p.AccountBlock(d.View, now) {
			fmt.Fprintln(out, line)
		}
	}
	return 0
}
