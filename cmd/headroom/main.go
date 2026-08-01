// headroom — Claude Code's /usage view, outside Claude Code, across
// accounts, plus an interactive account picker. The Bash reference
// implementation lives in ~/dotfiles (scripts/.local/bin/claude-usage);
// system model in ~/dotfiles/docs/claude-accounts.md.
package main

import (
	"os"

	"github.com/qiushiyan/headroom/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
